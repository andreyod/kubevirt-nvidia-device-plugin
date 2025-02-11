package device_plugin

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	klog "k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	nvidiaVendorID    = "10de"
	basePath          = "/sys/bus/pci/devices"
	pciIdsFilePath    = "/usr/pci.ids"
	deviceIDSeparator = "|"
)

type PCIDevice struct {
	pciAddress string
	iommuGroup string
}

var stop = make(chan struct{})

func InitiateDevicePlugin() {
	//Identifies Nvidia devices and represents it in appropriate structures
	deviceMap := discoverPCIDevices()
	//Creates and starts device plugin for each Nvidia device type
	createDevicePlugins(deviceMap)
}

func createDevicePlugins(deviceMap map[string][]*PCIDevice) {
	var devicePlugins []*GenericDevicePlugin
	var devs []*pluginapi.Device
	log.Printf("Device Map %v", deviceMap)

	//Iterate over deivceMap to create device plugin for each type of GPU on the host
	for id, devices := range deviceMap {
		devs = nil
		iommuToPCIMap := make(map[string]string)
		for _, device := range devices {
			deviceID := strings.Join([]string{device.iommuGroup, device.pciAddress}, deviceIDSeparator)
			iommuToPCIMap[deviceID] = device.pciAddress
			devs = append(devs, &pluginapi.Device{
				ID:     deviceID,
				Health: pluginapi.Healthy,
			})
		}
		deviceName := getDeviceName(id)
		if deviceName == "" {
			log.Printf("Error: Could not find device name for device id: %s", id)
			deviceName = id
		}
		log.Printf("DP Name %s", deviceName)
		log.Printf("Iommu Map %s", iommuToPCIMap)
		dp := NewGenericDevicePlugin(deviceName, "/dev/vfio/", devs, iommuToPCIMap)
		err := startDevicePlugin(dp)
		if err != nil {
			log.Printf("Error starting %s device plugin: %v", dp.deviceName, err)
		} else {
			devicePlugins = append(devicePlugins, dp)
		}
	}
	<-stop
	log.Printf("Shutting down device plugin controller")
	for _, v := range devicePlugins {
		v.Stop()
	}

}

func startDevicePlugin(dp *GenericDevicePlugin) error {
	return dp.Start(stop)
}

func discoverPCIDevices() map[string][]*PCIDevice {

	pciDevicesMap := make(map[string][]*PCIDevice)

	//Walk directory to discover pci devices
	filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("Error accessing file path %q: %v\n", path, err)
			return err
		}
		if info.IsDir() {
			log.Println("Not a device, continuing")
			return nil
		}
		//Retrieve vendor for the device
		vendorID, err := readIDFromFile(basePath, info.Name(), "vendor")
		if err != nil {
			log.Println("Could not get vendor ID for device ", info.Name())
			return nil
		}

		//Nvidia vendor id is "10de". Proceed if vendor id is 10de
		if vendorID == "10de" {
			log.Println("Nvidia device ", info.Name())
			//Retrieve iommu group for the device
			driver, err := readLink(basePath, info.Name(), "driver")
			if err != nil {
				log.Println("Could not get driver for device ", info.Name())
				return nil
			}
			if driver == "vfio-pci" {
				iommuGroup, err := readLink(basePath, info.Name(), "iommu_group")
				if err != nil {
					log.Println("Could not get IOMMU Group for device ", info.Name())
					return nil
				}
				log.Println("Iommu Group " + iommuGroup)
				deviceID, err := readIDFromFile(basePath, info.Name(), "device")
				if err != nil {
					log.Println("Could get deviceID for PCI address ", info.Name())
					return nil
				}
				log.Printf("Device Id %s", deviceID)
				pcidev := &PCIDevice{
					pciAddress: info.Name(),
					iommuGroup: iommuGroup,
				}
				pciDevicesMap[deviceID] = append(pciDevicesMap[deviceID], pcidev)
			}
		}
		return nil
	})
	return pciDevicesMap
}

func readIDFromFile(basePath string, deviceAddress string, property string) (string, error) {
	data, err := os.ReadFile(filepath.Join(basePath, deviceAddress, property))
	if err != nil {
		klog.Errorf("Could not read %s for device %s: %s", property, deviceAddress, err)
		return "", err
	}
	id := strings.Trim(string(data[2:]), "\n")
	return id, nil
}

func readLink(basePath string, deviceAddress string, link string) (string, error) {
	path, err := os.Readlink(filepath.Join(basePath, deviceAddress, link))
	if err != nil {
		klog.Errorf("Could not read link %s for device %s: %s", link, deviceAddress, err)
		return "", err
	}
	_, file := filepath.Split(path)
	return file, nil
}

func getDeviceName(deviceID string) string {
	deviceName := ""
	file, err := os.Open(pciIdsFilePath)
	if err != nil {
		log.Printf("Error opening pci ids file %s", pciIdsFilePath)
		return ""
	}
	defer file.Close()

	// Locate beginning of NVIDIA device list in pci.ids file
	scanner, err := locateVendor(file, nvidiaVendorID)
	if err != nil {
		log.Printf("Error locating NVIDIA in pci.ds file: %v", err)
		return ""
	}

	// Find NVIDIA device by device id
	prefix := fmt.Sprintf("\t%s", deviceID)
	for scanner.Scan() {
		line := scanner.Text()
		// ignore comments
		if strings.HasPrefix(line, "#") {
			continue
		}
		// if line does not start with tab, we are visiting a different vendor
		if !strings.HasPrefix(line, "\t") {
			log.Printf("Could not find NVIDIA device with id: %s", deviceID)
			return ""
		}
		if !strings.HasPrefix(line, prefix) {
			continue
		}

		deviceName = strings.TrimPrefix(line, prefix)
		deviceName = strings.TrimSpace(deviceName)
		deviceName = strings.ToUpper(deviceName)
		deviceName = strings.Replace(deviceName, "/", "_", -1)
		deviceName = strings.Replace(deviceName, ".", "_", -1)
		// Replace all spaces with underscore
		reg, _ := regexp.Compile("\\s+")
		deviceName = reg.ReplaceAllString(deviceName, "_")
		// Removes any char other than alphanumeric and underscore
		reg, _ = regexp.Compile("[^a-zA-Z0-9_.]+")
		deviceName = reg.ReplaceAllString(deviceName, "")
		break
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Error reading pci ids file %s", err)
	}
	return deviceName
}

func locateVendor(pciIdsFile *os.File, vendorID string) (*bufio.Scanner, error) {
	scanner := bufio.NewScanner(pciIdsFile)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, vendorID) {
			return scanner, nil
		}
	}

	if err := scanner.Err(); err != nil {
		return scanner, fmt.Errorf("error reading pci.ids file: %v", err)
	}

	return scanner, fmt.Errorf("failed to find vendor id in pci.ids file: %s", vendorID)
}
