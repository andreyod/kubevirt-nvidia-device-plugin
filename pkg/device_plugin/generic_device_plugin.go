package device_plugin

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	DeviceNamespace     = "nvidia.com"
	connectionTimeout   = 5 * time.Second
	vfioDevicePath      = "/dev/vfio"
	gpuPrefix           = "PCI_RESOURCE_NVIDIA_COM"
	partitionDataSource = "/var/lib/kubelet/device-plugins/partition-service"
)

type DevicePluginBase struct {
	devs       []*pluginapi.Device
	server     *grpc.Server
	socketPath string
	stop       chan struct{} // this channel signals to stop the DP
	term       chan bool     // this channel detects kubelet restarts
	healthy    chan string
	unhealthy  chan string
	devicePath string
	deviceName string
	devsHealth []*pluginapi.Device
}

type PCIDevicePlugin struct {
	*DevicePluginBase
	iommuToPCIMap map[string]string
}

// Implements the kubernetes device plugin API
type GenericDevicePlugin struct {
	devs          []*pluginapi.Device
	server        *grpc.Server
	socketPath    string
	stop          chan struct{} // this channel signals to stop the DP
	term          chan bool     // this channel detects kubelet restarts
	healthy       chan string
	unhealthy     chan string
	devicePath    string
	deviceName    string
	devsHealth    []*pluginapi.Device
	iommuToPCIMap map[string]string
}

// Returns an initialized instance of GenericDevicePlugin
func NewGenericDevicePlugin(deviceName string, devicePath string, devices []*pluginapi.Device, iommuToPCIMap map[string]string) *GenericDevicePlugin {
	//log.Println("Devicename " + deviceName)
	serverSock := fmt.Sprintf(pluginapi.DevicePluginPath+"kubevirt-%s.sock", deviceName)
	dpi := &GenericDevicePlugin{
		devs:          devices,
		socketPath:    serverSock,
		term:          make(chan bool, 1),
		healthy:       make(chan string),
		unhealthy:     make(chan string),
		deviceName:    deviceName,
		devicePath:    devicePath,
		iommuToPCIMap: iommuToPCIMap,
	}
	return dpi
}

func buildEnv(envList map[string][]string) map[string]string {
	env := map[string]string{}
	for key, devList := range envList {
		env[key] = strings.Join(devList, ",")
	}
	return env
}

func waitForGrpcServer(socketPath string, timeout time.Duration) error {
	conn, err := connect(socketPath, timeout)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// dial establishes the gRPC communication with the registered device plugin.
func connect(socketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	ctx, _ := context.WithTimeout(context.Background(), timeout)
	c, err := grpc.DialContext(ctx, socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			if deadline, ok := ctx.Deadline(); ok {
				return net.DialTimeout("unix", addr, time.Until(deadline))
			}
			return net.DialTimeout("unix", addr, connectionTimeout)
		}),
	)
	if err != nil {
		return nil, err
	}

	return c, nil
}

// Start starts the gRPC server of the device plugin
func (dpi *GenericDevicePlugin) Start(stop chan struct{}) error {
	if dpi.server != nil {
		return fmt.Errorf("gRPC server already started")
	}

	dpi.stop = stop

	err := dpi.cleanup()
	if err != nil {
		return err
	}

	sock, err := net.Listen("unix", dpi.socketPath)
	if err != nil {
		log.Printf("[%s] Error creating GRPC server socket: %v", dpi.deviceName, err)
		return err
	}

	dpi.server = grpc.NewServer([]grpc.ServerOption{}...)
	pluginapi.RegisterDevicePluginServer(dpi.server, dpi)

	go dpi.server.Serve(sock)

	err = waitForGrpcServer(dpi.socketPath, connectionTimeout)
	if err != nil {
		// this err is returned at the end of the Start function
		log.Printf("[%s] Error connecting to GRPC server: %v", dpi.deviceName, err)
	}

	err = dpi.Register()
	if err != nil {
		log.Printf("[%s] Error registering with device plugin manager: %v", dpi.deviceName, err)
		return err
	}

	go dpi.healthCheck()

	log.Println(dpi.deviceName + " Device plugin server ready")

	return err
}

// Stop stops the gRPC server
func (dpi *GenericDevicePlugin) Stop() error {
	if dpi.server == nil {
		return nil
	}

	// Send terminate signal to ListAndWatch()
	dpi.term <- true

	dpi.server.Stop()
	dpi.server = nil

	return dpi.cleanup()
}

// Restarts DP server
func (dpi *GenericDevicePlugin) restart() error {
	log.Printf("Restarting %s device plugin server", dpi.deviceName)
	if dpi.server == nil {
		return fmt.Errorf("grpc server instance not found for %s", dpi.deviceName)
	}

	dpi.Stop()

	// Create new instance of a grpc server
	var stop = make(chan struct{})
	return dpi.Start(stop)
}

// Register registers the device plugin for the given resourceName with Kubelet.
func (dpi *GenericDevicePlugin) Register() error {
	conn, err := connect(pluginapi.KubeletSocket, connectionTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)
	reqt := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(dpi.socketPath),
		ResourceName: fmt.Sprintf("%s/%s", DeviceNamespace, dpi.deviceName),
	}

	_, err = client.Register(context.Background(), reqt)
	if err != nil {
		return err
	}
	return nil
}

// ListAndWatch lists devices and update that list according to the health status
func (dpi *GenericDevicePlugin) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {

	s.Send(&pluginapi.ListAndWatchResponse{Devices: dpi.devs})

	for {
		select {
		case unhealthy := <-dpi.unhealthy:
			log.Printf("In watch unhealthy")
			for _, dev := range dpi.devs {
				if unhealthy == dev.ID {
					dev.Health = pluginapi.Unhealthy
				}
			}
			s.Send(&pluginapi.ListAndWatchResponse{Devices: dpi.devs})
		case healthy := <-dpi.healthy:
			log.Printf("In watch healthy")
			for _, dev := range dpi.devs {
				if healthy == dev.ID {
					dev.Health = pluginapi.Healthy
				}
			}
			s.Send(&pluginapi.ListAndWatchResponse{Devices: dpi.devs})
		case <-dpi.stop:
			return nil
		case <-dpi.term:
			return nil
		}
	}
}

// func (dpi *GenericDevicePlugin) Allocate(_ context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
// 	resourceNameEnvVar := fmt.Sprintf("%s_%s", gpuPrefix, strings.ToUpper(dpi.deviceName))
// 	allocatedDevices := []string{}
// 	resp := new(pluginapi.AllocateResponse)
// 	containerResponse := new(pluginapi.ContainerAllocateResponse)

// 	for _, request := range r.ContainerRequests {
// 		deviceSpecs := make([]*pluginapi.DeviceSpec, 0)
// 		for _, devID := range request.DevicesIDs {
// 			log.Printf("------ devID from the request - %s", devID)
// 			// translate device's iommu group to its pci address
// 			devPCIAddress, exist := dpi.iommuToPCIMap[devID]
// 			if !exist {
// 				log.Printf("Missing device mapping for %s", devID)
// 				continue
// 			}
// 			allocatedDevices = append(allocatedDevices, devPCIAddress)
// 			deviceSpecs = append(deviceSpecs, formatDeviceSpecs(devID)...)
// 		}
// 		containerResponse.Devices = deviceSpecs
// 		envVar := make(map[string]string)
// 		envVar[resourceNameEnvVar] = strings.Join(allocatedDevices, ",")

// 		log.Printf("Allocated devices %s", envVar)
// 		containerResponse.Envs = envVar
// 		resp.ContainerResponses = append(resp.ContainerResponses, containerResponse)
// 	}
// 	return resp, nil
// }

func (dpi *GenericDevicePlugin) Allocate(_ context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	resourceNameEnvVar := fmt.Sprintf("%s_%s", gpuPrefix, strings.ToUpper(dpi.deviceName))
	allocatedDevices := []string{}
	resp := new(pluginapi.AllocateResponse)
	containerResponse := new(pluginapi.ContainerAllocateResponse)
	partitionID := ""
	for _, request := range r.ContainerRequests {
		deviceSpecs := make([]*pluginapi.DeviceSpec, 0)
		for _, devID := range request.DevicesIDs { // temp debug
			log.Printf("------ devID from the request - %s", devID)
		}
		devicesIDs := request.DevicesIDs

		if !strings.Contains(dpi.deviceName, "NVSwitch") {
			log.Println("Activating GPU Partition")
			ip, err := getPartitionManagerIP(partitionDataSource)
			if err != nil || ip == "" {
				log.Printf("Partition Manager IP was not found: %v", err)
			} else { // try to activate partition
				activateLines, err := activatePartition(len(request.DevicesIDs), ip)
				if err != nil {
					log.Printf("Activate GPU partition error: %v", err)
					return nil, err
				}
				if len(activateLines) < 3 {
					log.Printf("Failed to activate partition: %v", activateLines)
					return nil, fmt.Errorf("Failed to activate GPU partition")
				}
				// parse the Activate Partition output to get PCI and partition ID.
				partitionPrefix := "PartitionID"
				pciPrefix := "00"
				partitionDevices := []string{}
				for _, line := range activateLines {
					if strings.HasPrefix(line, pciPrefix) {
						devicePCI := strings.SplitN(strings.Split(line, " ")[0], ":", 2)[1]
						log.Printf("PCI from Activated Partition: %s", devicePCI)
						partitionDevices = append(partitionDevices, strings.ToLower(devicePCI))
					}
					if strings.HasPrefix(line, partitionPrefix) {
						partitionID = strings.Split(line, " ")[1]
						log.Printf("Activated Partition ID: %s", partitionID)
					}
				}
				if len(partitionDevices) != len(devicesIDs) {
					log.Printf("Mismatch: partition has %s devices but want %s", len(partitionDevices), len(devicesIDs))
					return nil, fmt.Errorf("Mismatch: partition has %s devices but want %s", len(partitionDevices), len(devicesIDs))
				}
				// find DevicesIDs for partitionDevices
				partitionDeviceIDs := []string{}
				for _, dev := range partitionDevices {
					for k, v := range dpi.iommuToPCIMap {
						if strings.Contains(v, dev) {
							partitionDeviceIDs = append(partitionDeviceIDs, k)
						}
					}
				}
				if len(partitionDeviceIDs) != len(devicesIDs) || partitionID == "" {
					log.Printf("Failed to activate %d GPU partition: %v", len(devicesIDs), activateLines)
					return nil, fmt.Errorf("Failed to activate GPU partition")
				}
				devicesIDs = partitionDeviceIDs
			}
		}
		for _, devID := range devicesIDs {
			// translate device's iommu group to its pci address
			devPCIAddress, exist := dpi.iommuToPCIMap[devID]
			if !exist {
				log.Printf("Missing device mapping for %s", devID)
				continue
			}
			allocatedDevices = append(allocatedDevices, devPCIAddress)
			deviceSpecs = append(deviceSpecs, formatDeviceSpecs(devID)...)
		}

		containerResponse.Devices = deviceSpecs
		envVar := make(map[string]string)
		envVar[resourceNameEnvVar] = strings.Join(allocatedDevices, ",")
		if partitionID != "" {
			annotations := make(map[string]string)
			annotations["partitionID"] = partitionID
			containerResponse.Annotations = annotations
			log.Println("Annotated partitionID")
		}

		log.Printf("Allocated devices %s", envVar)
		containerResponse.Envs = envVar
		resp.ContainerResponses = append(resp.ContainerResponses, containerResponse)
	}
	return resp, nil
}

func activatePartition(numberOfGPUs int, partitionManagerIP string) ([]string, error) {

	activateCommand := `echo "A ` + fmt.Sprint(numberOfGPUs) + `" | nc ` + partitionManagerIP + ` 8080`
	// Create the command: echo "A 4" | nc 10.131.2.107 8080
	// cmd := exec.Command("sh", "-c", `echo "A 4" | nc 10.131.2.107 8080`)
	cmd := exec.Command("sh", "-c", activateCommand)

	log.Printf("Run activate partition command: %s", activateCommand)

	// Capture stdout
	var out bytes.Buffer
	cmd.Stdout = &out
	// Run the command
	err := cmd.Run()
	if err != nil {
		return nil, err
	}
	// Read the output line by line
	scanner := bufio.NewScanner(&out)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

func getPartitionManagerIP(file string) (string, error) {
	var nodename string
	ips, err := readLines(file)
	if err != nil {
		return "", err
	} else {
		nodename = os.Getenv("NODE_NAME")
		if nodename == "" {
			return "", fmt.Errorf("NODE_NAME env var is not set")
		}
		for _, ip := range ips {
			if strings.HasPrefix(ip, nodename) {
				return strings.Split(ip, " ")[1], nil
			}
		}
	}
	return "", fmt.Errorf("PartitionManager IP for node %s was not found in %s", nodename, file)
}

func readLines(filePath string) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return lines, nil
}

func (dpi *GenericDevicePlugin) cleanup() error {
	if err := os.Remove(dpi.socketPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

func (dpi *GenericDevicePlugin) GetDevicePluginOptions(ctx context.Context, e *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	options := &pluginapi.DevicePluginOptions{
		PreStartRequired: false,
	}
	return options, nil
}

func (dpi *GenericDevicePlugin) PreStartContainer(ctx context.Context, in *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	res := &pluginapi.PreStartContainerResponse{}
	return res, nil
}

// GetPreferredAllocation is for compatible with new DevicePluginServer API for DevicePlugin service. It has not been implemented in kubevrit-gpu-device-plugin
func (dpi *GenericDevicePlugin) GetPreferredAllocation(ctx context.Context, in *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	// TODO
	// returns a preferred set of devices to allocate
	// from a list of available ones. The resulting preferred allocation is not
	// guaranteed to be the allocation ultimately performed by the
	// devicemanager. It is only designed to help the devicemanager make a more
	// informed allocation decision when possible.
	return nil, nil
}

// Health check of GPU devices
func (dpi *GenericDevicePlugin) healthCheck() error {
	method := fmt.Sprintf("healthCheck(%s)", dpi.deviceName)
	log.Printf("%s: invoked", method)
	var pathDeviceMap = make(map[string]string)
	var path = dpi.devicePath
	var health = ""

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("%s: Unable to create fsnotify watcher: %v", method, err)
		return err
	}
	defer watcher.Close()

	err = watcher.Add(filepath.Dir(dpi.socketPath))
	if err != nil {
		log.Printf("%s: Unable to add device plugin socket path to fsnotify watcher: %v", method, err)
		return err
	}

	_, err = os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("%s: Unable to stat device: %v", method, err)
			return err
		}
	}

	for _, dev := range dpi.devs {
		iommuGroup := strings.Split(dev.ID, deviceIDSeparator)[0]
		devicePath := filepath.Join(path, iommuGroup)
		err = watcher.Add(devicePath)
		pathDeviceMap[devicePath] = dev.ID
		if err != nil {
			log.Printf("%s: Unable to add device path to fsnotify watcher: %v", method, err)
			return err
		}
	}

	for {
		select {
		case <-dpi.stop:
			return nil
		case event := <-watcher.Events:
			v, ok := pathDeviceMap[event.Name]
			if ok {
				// Health in this case is if the device path actually exists
				if event.Op == fsnotify.Create {
					health = v
					dpi.healthy <- health
				} else if (event.Op == fsnotify.Remove) || (event.Op == fsnotify.Rename) {
					log.Printf("%s: Marking device unhealthy: %s", method, event.Name)
					health = v
					dpi.unhealthy <- health
				}
			} else if event.Name == dpi.socketPath && event.Op == fsnotify.Remove {
				// Watcher event for removal of socket file
				log.Printf("%s: Socket path for GPU device was removed, kubelet likely restarted", method)
				// Trigger restart of the DP servers
				if err := dpi.restart(); err != nil {
					log.Printf("%s: Unable to restart server %v", method, err)
					return err
				}
				log.Printf("%s: Successfully restarted %s device plugin server. Terminating.", method, dpi.deviceName)
				return nil
			}
		}
	}
}

func formatDeviceSpecs(devID string) []*pluginapi.DeviceSpec {
	// always add /dev/vfio/vfio device as well
	devSpecs := make([]*pluginapi.DeviceSpec, 0)
	devSpecs = append(devSpecs, &pluginapi.DeviceSpec{
		HostPath:      filepath.Join(vfioDevicePath, "vfio"),
		ContainerPath: filepath.Join(vfioDevicePath, "vfio"),
		Permissions:   "mrw",
	})
	iommuGroup := strings.Split(devID, deviceIDSeparator)[0]
	vfioDevice := filepath.Join(vfioDevicePath, iommuGroup)
	devSpecs = append(devSpecs, &pluginapi.DeviceSpec{
		HostPath:      vfioDevice,
		ContainerPath: vfioDevice,
		Permissions:   "mrw",
	})
	return devSpecs
}
