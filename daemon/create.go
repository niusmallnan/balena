package daemon

import (
	"archive/tar"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/Sirupsen/logrus"
	apierrors "github.com/docker/docker/api/errors"
	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	networktypes "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/container"
	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/progress"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/pkg/streamformatter"
	"github.com/docker/docker/pkg/system"
	"github.com/docker/docker/runconfig"
	units "github.com/docker/go-units"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/resin-os/librsync-go"
)

// CreateManagedContainer creates a container that is managed by a Service
func (daemon *Daemon) CreateManagedContainer(params types.ContainerCreateConfig) (containertypes.ContainerCreateCreatedBody, error) {
	return daemon.containerCreate(params, true)
}

// ContainerCreate creates a regular container
func (daemon *Daemon) ContainerCreate(params types.ContainerCreateConfig) (containertypes.ContainerCreateCreatedBody, error) {
	return daemon.containerCreate(params, false)
}

func (daemon *Daemon) containerCreate(params types.ContainerCreateConfig, managed bool) (containertypes.ContainerCreateCreatedBody, error) {
	start := time.Now()
	if params.Config == nil {
		return containertypes.ContainerCreateCreatedBody{}, fmt.Errorf("Config cannot be empty in order to create a container")
	}

	warnings, err := daemon.verifyContainerSettings(params.HostConfig, params.Config, false)
	if err != nil {
		return containertypes.ContainerCreateCreatedBody{Warnings: warnings}, err
	}

	err = daemon.verifyNetworkingConfig(params.NetworkingConfig)
	if err != nil {
		return containertypes.ContainerCreateCreatedBody{Warnings: warnings}, err
	}

	if params.HostConfig == nil {
		params.HostConfig = &containertypes.HostConfig{}
	}
	err = daemon.adaptContainerSettings(params.HostConfig, params.AdjustCPUShares)
	if err != nil {
		return containertypes.ContainerCreateCreatedBody{Warnings: warnings}, err
	}

	container, err := daemon.create(params, managed)
	if err != nil {
		return containertypes.ContainerCreateCreatedBody{Warnings: warnings}, daemon.imageNotExistToErrcode(err)
	}
	containerActions.WithValues("create").UpdateSince(start)

	return containertypes.ContainerCreateCreatedBody{ID: container.ID, Warnings: warnings}, nil
}

// Create creates a new container from the given configuration with a given name.
func (daemon *Daemon) create(params types.ContainerCreateConfig, managed bool) (retC *container.Container, retErr error) {
	var (
		container *container.Container
		img       *image.Image
		imgID     image.ID
		err       error
	)

	// TODO: @jhowardmsft LCOW support - at a later point, can remove the hard-coding
	// to force the platform to be linux.
	// Default the platform if not supplied
	if params.Platform == "" {
		params.Platform = runtime.GOOS
	}
	if system.LCOWSupported() {
		params.Platform = "linux"
	}

	if params.Config.Image != "" {
		img, err = daemon.GetImage(params.Config.Image)
		if err != nil {
			return nil, err
		}

		if runtime.GOOS == "solaris" && img.OS != "solaris " {
			return nil, errors.New("platform on which parent image was created is not Solaris")
		}
		imgID = img.ID()

		if runtime.GOOS == "windows" && img.OS == "linux" && !system.LCOWSupported() {
			return nil, errors.New("platform on which parent image was created is not Windows")
		}
	}

	// Make sure the platform requested matches the image
	if img != nil {
		if params.Platform != img.Platform() {
			// Ignore this in LCOW mode. @jhowardmsft TODO - This will need revisiting later.
			if !system.LCOWSupported() {
				return nil, fmt.Errorf("cannot create a %s container from a %s image", params.Platform, img.Platform())
			}
		}
	}

	if err := daemon.mergeAndVerifyConfig(params.Config, img); err != nil {
		return nil, err
	}

	if err := daemon.mergeAndVerifyLogConfig(&params.HostConfig.LogConfig); err != nil {
		return nil, err
	}

	if container, err = daemon.newContainer(params.Name, params.Platform, params.Config, params.HostConfig, imgID, managed); err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			if err := daemon.cleanupContainer(container, true, true); err != nil {
				logrus.Errorf("failed to cleanup container on create error: %v", err)
			}
		}
	}()

	if err := daemon.setSecurityOptions(container, params.HostConfig); err != nil {
		return nil, err
	}

	container.HostConfig.StorageOpt = params.HostConfig.StorageOpt

	// Set RWLayer for container after mount labels have been set
	if err := daemon.setRWLayer(container, params.HostConfig.Runtime); err != nil {
		return nil, err
	}

	rootIDs := daemon.idMappings.RootPair()
	if err := idtools.MkdirAndChown(container.Root, 0700, rootIDs); err != nil {
		return nil, err
	}
	if err := idtools.MkdirAndChown(container.CheckpointDir(), 0700, rootIDs); err != nil {
		return nil, err
	}

	if err := daemon.setHostConfig(container, params.HostConfig); err != nil {
		return nil, err
	}

	if err := daemon.createContainerPlatformSpecificSettings(container, params.Config, params.HostConfig); err != nil {
		return nil, err
	}

	var endpointsConfigs map[string]*networktypes.EndpointSettings
	if params.NetworkingConfig != nil {
		endpointsConfigs = params.NetworkingConfig.EndpointsConfig
	}
	// Make sure NetworkMode has an acceptable value. We do this to ensure
	// backwards API compatibility.
	runconfig.SetDefaultNetModeIfBlank(container.HostConfig)

	daemon.updateContainerNetworkSettings(container, endpointsConfigs)
	if err := daemon.Register(container); err != nil {
		return nil, err
	}
	stateCtr.set(container.ID, "stopped")
	daemon.LogContainerEvent(container, "create")
	return container, nil
}

func toHostConfigSelinuxLabels(labels []string) []string {
	for i, l := range labels {
		labels[i] = "label=" + l
	}
	return labels
}

func (daemon *Daemon) generateSecurityOpt(hostConfig *containertypes.HostConfig) ([]string, error) {
	for _, opt := range hostConfig.SecurityOpt {
		con := strings.Split(opt, "=")
		if con[0] == "label" {
			// Caller overrode SecurityOpts
			return nil, nil
		}
	}
	ipcMode := hostConfig.IpcMode
	pidMode := hostConfig.PidMode
	privileged := hostConfig.Privileged
	if ipcMode.IsHost() || pidMode.IsHost() || privileged {
		return toHostConfigSelinuxLabels(label.DisableSecOpt()), nil
	}

	var ipcLabel []string
	var pidLabel []string
	ipcContainer := ipcMode.Container()
	pidContainer := pidMode.Container()
	if ipcContainer != "" {
		c, err := daemon.GetContainer(ipcContainer)
		if err != nil {
			return nil, err
		}
		ipcLabel = label.DupSecOpt(c.ProcessLabel)
		if pidContainer == "" {
			return toHostConfigSelinuxLabels(ipcLabel), err
		}
	}
	if pidContainer != "" {
		c, err := daemon.GetContainer(pidContainer)
		if err != nil {
			return nil, err
		}

		pidLabel = label.DupSecOpt(c.ProcessLabel)
		if ipcContainer == "" {
			return toHostConfigSelinuxLabels(pidLabel), err
		}
	}

	if pidLabel != nil && ipcLabel != nil {
		for i := 0; i < len(pidLabel); i++ {
			if pidLabel[i] != ipcLabel[i] {
				return nil, fmt.Errorf("--ipc and --pid containers SELinux labels aren't the same")
			}
		}
		return toHostConfigSelinuxLabels(pidLabel), nil
	}
	return nil, nil
}

func (daemon *Daemon) setRWLayer(container *container.Container, runtime string) error {
	var layerID layer.ChainID
	if container.ImageID != "" {
		img, err := daemon.stores[container.Platform].imageStore.Get(container.ImageID)
		if err != nil {
			return err
		}
		layerID = img.RootFS.ChainID()
	}

	initFunc := daemon.getLayerInit()
	if  runtime == "bare" {
		initFunc = nil
	}

	rwLayerOpts := &layer.CreateRWLayerOpts{
		MountLabel: container.MountLabel,
		InitFunc:   initFunc,
		StorageOpt: container.HostConfig.StorageOpt,
	}

	rwLayer, err := daemon.stores[container.Platform].layerStore.CreateRWLayer(container.ID, layerID, rwLayerOpts)
	if err != nil {
		return err
	}
	container.RWLayer = rwLayer

	return nil
}

// VolumeCreate creates a volume with the specified name, driver, and opts
// This is called directly from the Engine API
func (daemon *Daemon) VolumeCreate(name, driverName string, opts, labels map[string]string) (*types.Volume, error) {
	if name == "" {
		name = stringid.GenerateNonCryptoID()
	}

	v, err := daemon.volumes.Create(name, driverName, opts, labels)
	if err != nil {
		return nil, err
	}

	daemon.LogVolumeEvent(v.Name(), "create", map[string]string{"driver": v.DriverName()})
	apiV := volumeToAPIType(v)
	apiV.Mountpoint = v.Path()
	return apiV, nil
}

func (daemon *Daemon) mergeAndVerifyConfig(config *containertypes.Config, img *image.Image) error {
	if img != nil && img.Config != nil {
		if err := merge(config, img.Config); err != nil {
			return err
		}
	}
	// Reset the Entrypoint if it is [""]
	if len(config.Entrypoint) == 1 && config.Entrypoint[0] == "" {
		config.Entrypoint = nil
	}
	if len(config.Entrypoint) == 0 && len(config.Cmd) == 0 {
		return fmt.Errorf("No command specified")
	}
	return nil
}

// Checks if the client set configurations for more than one network while creating a container
// Also checks if the IPAMConfig is valid
func (daemon *Daemon) verifyNetworkingConfig(nwConfig *networktypes.NetworkingConfig) error {
	if nwConfig == nil || len(nwConfig.EndpointsConfig) == 0 {
		return nil
	}
	if len(nwConfig.EndpointsConfig) == 1 {
		for _, v := range nwConfig.EndpointsConfig {
			if v != nil && v.IPAMConfig != nil {
				if v.IPAMConfig.IPv4Address != "" && net.ParseIP(v.IPAMConfig.IPv4Address).To4() == nil {
					return apierrors.NewBadRequestError(fmt.Errorf("invalid IPv4 address: %s", v.IPAMConfig.IPv4Address))
				}
				if v.IPAMConfig.IPv6Address != "" {
					n := net.ParseIP(v.IPAMConfig.IPv6Address)
					// if the address is an invalid network address (ParseIP == nil) or if it is
					// an IPv4 address (To4() != nil), then it is an invalid IPv6 address
					if n == nil || n.To4() != nil {
						return apierrors.NewBadRequestError(fmt.Errorf("invalid IPv6 address: %s", v.IPAMConfig.IPv6Address))
					}
				}
			}
		}
		return nil
	}
	l := make([]string, 0, len(nwConfig.EndpointsConfig))
	for k := range nwConfig.EndpointsConfig {
		l = append(l, k)
	}
	err := fmt.Errorf("Container cannot be connected to network endpoints: %s", strings.Join(l, ", "))
	return apierrors.NewBadRequestError(err)
}

// DeltaCreate creates a delta of the specified src and dest images
// This is called directly from the Engine API
func (daemon *Daemon) DeltaCreate(deltaSrc, deltaDest string, outStream io.Writer) error {
	progressOutput := streamformatter.NewJSONProgressOutput(outStream, false)

	srcImg, err := daemon.GetImage(deltaSrc)
	if err != nil {
		return errors.Wrapf(err, "no such image: %s", deltaSrc)
	}

	dstImg, err := daemon.GetImage(deltaDest)
	if err != nil {
		return errors.Wrapf(err, "no such image: %s", deltaDest)
	}

	is := daemon.stores[dstImg.Platform()].imageStore
	ls := daemon.stores[dstImg.Platform()].layerStore

	srcData, err := is.GetTarSeekStream(srcImg.ID())
	if err != nil {
		return err
	}
	defer srcData.Close()

	srcDataLen, err := ioutils.SeekerSize(srcData)
	if err != nil {
		return err
	}

	progressReader := progress.NewProgressReader(srcData, progressOutput, srcDataLen, deltaSrc, "Fingerprinting")
	defer progressReader.Close()

	srcSig, err := librsync.Signature(bufio.NewReaderSize(progressReader, 65536), ioutil.Discard, 512, 32, librsync.BLAKE2_SIG_MAGIC)
	if err != nil {
		return err
	}

	progress.Update(progressOutput, deltaSrc, "Fingerprint complete")

	deltaRootFS := image.NewRootFS()

	for _, diffID := range dstImg.RootFS.DiffIDs {
		progress.Update(progressOutput, stringid.TruncateID(diffID.String()), "Waiting")
	}

	statTotalSize := int64(0)
	statDeltaSize := int64(0)

	for i, diffID := range dstImg.RootFS.DiffIDs {
		var (
			layerData io.Reader
			platform layer.Platform
		)
		commonLayer := false

		// We're only interested in layers that are different. Push empty
		// layers for common layers
		if i < len(srcImg.RootFS.DiffIDs) && srcImg.RootFS.DiffIDs[i] == diffID {
			commonLayer = true
			layerData, _ = layer.EmptyLayer.TarStream()
			platform = layer.EmptyLayer.Platform()
		} else {
			dstRootFS := *dstImg.RootFS
			dstRootFS.DiffIDs = dstRootFS.DiffIDs[:i+1]

			l, err := ls.Get(dstRootFS.ChainID())
			if err != nil {
				return err
			}
			defer layer.ReleaseAndLog(ls, l)

			platform = l.Platform()

			input, err := l.TarStream()
			if err != nil {
				return err
			}
			defer input.Close()

			inputSize, err := l.DiffSize()
			if err != nil {
				return err
			}

			statTotalSize += inputSize

			progressReader := progress.NewProgressReader(input, progressOutput, inputSize, stringid.TruncateID(diffID.String()), "Computing delta")
			defer progressReader.Close()

			pR, pW := io.Pipe()

			layerData = pR

			tmpDelta, err := ioutil.TempFile("", "docker-delta-")
			if err != nil {
				return err
			}
			defer os.Remove(tmpDelta.Name())

			go func() {
				w := bufio.NewWriter(tmpDelta)
				err := librsync.Delta(srcSig, bufio.NewReader(progressReader), w)
				if err != nil {
					pW.CloseWithError(err)
					return
				}
				w.Flush()

				info, err := tmpDelta.Stat()
				if err != nil {
					pW.CloseWithError(err)
					return
				}

				tw := tar.NewWriter(pW)

				hdr := &tar.Header{
					Name: "delta",
					Mode: 0600,
					Size: info.Size(),
				}

				if err := tw.WriteHeader(hdr); err != nil {
					pW.CloseWithError(err)
					return
				}

				if _, err := tmpDelta.Seek(0, io.SeekStart); err != nil {
					pW.CloseWithError(err)
					return
				}

				if _, err := io.Copy(tw, tmpDelta); err != nil {
					pW.CloseWithError(err)
					return
				}

				if err := tw.Close(); err != nil {
					pW.CloseWithError(err)
					return
				}

				pW.Close()
			}()
		}

		newLayer, err := ls.Register(layerData, deltaRootFS.ChainID(), platform)
		if err != nil {
			return err
		}
		defer layer.ReleaseAndLog(ls, newLayer)

		if commonLayer {
			progress.Update(progressOutput, stringid.TruncateID(diffID.String()), "Skipping common layer")
		} else {
			deltaSize, err := newLayer.DiffSize()
			if err != nil {
				return err
			}
			statDeltaSize += deltaSize
			progress.Update(progressOutput, stringid.TruncateID(diffID.String()), "Delta complete")
		}

		deltaRootFS.Append(newLayer.DiffID())
	}

	config := image.Image{
		RootFS: deltaRootFS,
		V1Image: image.V1Image{
			Created: time.Now().UTC(),
			Config: &containertypes.Config{
				Labels: map[string]string{
					"io.resin.delta.base": srcImg.ID().String(),
					"io.resin.delta.config": string(dstImg.RawJSON()),
				},
			},
		},
	}

	rawConfig, err := json.Marshal(config)
	if err != nil {
		return err
	}

	id, err := is.Create(rawConfig)
	if err != nil {
		return err
	}

	humanTotal := units.HumanSize(float64(statTotalSize))
	humanDelta := units.HumanSize(float64(statDeltaSize))
	deltaRatio := float64(statTotalSize) / float64(statDeltaSize)
	if statTotalSize == 0 {
		deltaRatio = 1
	}

	outStream.Write(streamformatter.FormatStatus("", "Normal size: %s, Delta size: %s, %.2fx improvement", humanTotal, humanDelta, deltaRatio))
	outStream.Write(streamformatter.FormatStatus("", "Created delta: %s", id.String()))
	return nil
}
