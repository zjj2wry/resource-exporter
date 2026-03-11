/*
Copyright 2021 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package numatopo

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"k8s.io/klog"
	"volcano.sh/apis/pkg/apis/nodeinfo/v1alpha1"

	"volcano.sh/resource-exporter/pkg/args"
)

const (
	// NVIDIA PCI Vendor ID
	NVIDIAVendorID = "0x10de"
	// AMD PCI Vendor ID
	AMDVendorID = "0x1002"
	// PCI Class for 3D controller
	PCI3DControllerClass = "0x0302"
	// PCI Class for VGA controller
	PCIVGAControllerClass = "0x0300"
)

// GPUNumaInfo 维护 GPU 的 NUMA 拓扑信息
type GPUNumaInfo struct {
	// GPU 索引 -> GPU 详细信息
	gpuDetail map[int]v1alpha1.GPUInfo

	// NUMA ID -> GPU 索引列表
	numa2GPUs map[int][]int
}

// GPUDevice 表示一个 GPU 设备
type GPUDevice struct {
	PCIBusID    string // PCI Bus ID，如 "0000:86:00.0"
	NUMANodeID  int    // NUMA 节点 ID
	MinorNumber int    // 设备 minor number
	UUID        string // GPU UUID
}

// NewGPUNumaInfo 初始化 GPUNumaInfo
func NewGPUNumaInfo() *GPUNumaInfo {
	return &GPUNumaInfo{
		gpuDetail: make(map[int]v1alpha1.GPUInfo),
		numa2GPUs: make(map[int][]int),
	}
}

// Name 返回资源类型名称
func (info *GPUNumaInfo) Name() string {
	return "nvidia.com/gpu"
}

// Update 更新 GPU NUMA 信息
func (info *GPUNumaInfo) Update(opt *args.Argument) NumaInfo {
	newInfo := NewGPUNumaInfo()

	// 1. 检测所有 GPU 设备
	gpuDevices, err := detectGPUDevices(opt.DevicePath)
	if err != nil {
		klog.Warningf("Failed to detect GPU devices: %v", err)
		// GPU 检测失败不应该导致整个 exporter 失败，返回空信息
		return nil
	}

	if len(gpuDevices) == 0 {
		klog.V(4).Infof("No GPU devices found on this node")
		// 如果之前有 GPU 信息，现在没有了，返回新信息以清空
		if len(info.gpuDetail) > 0 {
			return newInfo
		}
		return nil
	}

	// 2. 为每个 GPU 构建信息
	for idx, device := range gpuDevices {
		gpuInfo := v1alpha1.GPUInfo{
			PCIBusID:    device.PCIBusID,
			NUMANodeID:  device.NUMANodeID,
			MinorNumber: device.MinorNumber,
			UUID:        device.UUID,
		}

		newInfo.gpuDetail[idx] = gpuInfo
		newInfo.numa2GPUs[device.NUMANodeID] = append(newInfo.numa2GPUs[device.NUMANodeID], idx)

		klog.V(4).Infof("Detected GPU%d: PCI=%s, NUMA=%d, Minor=%d, UUID=%s",
			idx, device.PCIBusID, device.NUMANodeID, device.MinorNumber, device.UUID)
	}

	// 3. 检查是否有变化
	if !reflect.DeepEqual(newInfo, info) {
		klog.Infof("GPU topology changed: detected %d GPUs", len(gpuDevices))
		return newInfo
	}

	return nil
}

// detectGPUDevices 检测所有 GPU 设备并返回排序后的列表
func detectGPUDevices(sysDevicesPath string) ([]GPUDevice, error) {
	// 构建 PCI 设备路径
	// sysDevicesPath 是 /host/device，对应 /sys/devices/system
	// 需要访问 /sys/bus/pci/devices，所以是 /host/sys/bus/pci/devices
	pciDevicesPath := "/host/sys/bus/pci/devices"

	// 检查目录是否存在
	if _, err := os.Stat(pciDevicesPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("PCI devices directory not found: %s", pciDevicesPath)
	}

	// 扫描所有 PCI 设备
	entries, err := ioutil.ReadDir(pciDevicesPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read PCI devices directory: %v", err)
	}

	klog.V(4).Infof("Found %d PCI devices in %s", len(entries), pciDevicesPath)

	var pciDevicePaths []string
	for _, entry := range entries {
		pciDevicePaths = append(pciDevicePaths, filepath.Join(pciDevicesPath, entry.Name()))
	}

	// ⚠️ 关键：按 PCI Bus ID 排序以保证 GPU 索引稳定
	sort.Strings(pciDevicePaths)

	var gpuDevices []GPUDevice

	for _, devicePath := range pciDevicePaths {
		// 直接使用 PCI 设备路径，不需要解析符号链接
		// /host/sys/bus/pci/devices/<bus_id>/ 下的文件都可以直接访问

		// 检查是否为 GPU 设备
		isGPU, reason := isGPUDeviceWithReason(devicePath)
		if !isGPU {
			klog.V(6).Infof("Device %s is not GPU: %s", filepath.Base(devicePath), reason)
			continue
		}

		klog.V(4).Infof("Found GPU device: %s", filepath.Base(devicePath))

		// 读取 NUMA 节点信息
		numaNode, err := readNUMANode(devicePath)
		if err != nil {
			klog.Warningf("Failed to read NUMA node for GPU %s: %v", filepath.Base(devicePath), err)
			continue
		}

		// 获取 PCI Bus ID（从路径中提取）
		pciBusID := filepath.Base(devicePath)

		// 尝试获取 minor number 和 UUID（如果可用）
		minorNumber, uuid := getGPUDeviceInfo(pciBusID)

		gpuDevice := GPUDevice{
			PCIBusID:    pciBusID,
			NUMANodeID:  numaNode,
			MinorNumber: minorNumber,
			UUID:        uuid,
		}

		gpuDevices = append(gpuDevices, gpuDevice)
	}

	klog.Infof("Detected %d GPU devices", len(gpuDevices))
	return gpuDevices, nil
}

// resolveSysfsDeviceRealPath 解析 sysfs 设备符号链接到真实路径
func resolveSysfsDeviceRealPath(deviceSymlinkPath, sysDevicesPath string) (string, error) {
	fi, err := os.Lstat(deviceSymlinkPath)
	if err != nil {
		return "", err
	}

	// 如果不是符号链接，直接返回
	if (fi.Mode() & os.ModeSymlink) == 0 {
		return deviceSymlinkPath, nil
	}

	target, err := os.Readlink(deviceSymlinkPath)
	if err != nil {
		return "", err
	}

	// 处理相对路径 "../../../devices/..."
	// 链接通常指向 ../../../devices/pci0000:00/...
	// 我们需要转换为 /host/device/...
	if strings.HasPrefix(target, "../../../devices/") {
		rel := strings.TrimPrefix(target, "../../../devices/")
		// sysDevicesPath 是 /host/device，对应 /sys/devices/system
		// 所以上级目录 /sys/devices 对应 /host/device/../
		return filepath.Join(sysDevicesPath, "..", rel), nil
	}

	// 处理绝对路径 "/sys/devices/..."
	if strings.HasPrefix(target, "/sys/devices/") {
		rel := strings.TrimPrefix(target, "/sys/devices/")
		return filepath.Join(sysDevicesPath, "..", rel), nil
	}

	// 其他情况，相对路径拼接
	return filepath.Clean(filepath.Join(filepath.Dir(deviceSymlinkPath), target)), nil
}

// isGPUDevice 检查 PCI 设备是否为 GPU 设备
func isGPUDevice(devicePath string) bool {
	isGPU, _ := isGPUDeviceWithReason(devicePath)
	return isGPU
}

// isGPUDeviceWithReason 检查设备是否为 GPU 设备，并返回原因
func isGPUDeviceWithReason(devicePath string) (bool, string) {
	// 读取 PCI class
	classFile := filepath.Join(devicePath, "class")
	classData, err := ioutil.ReadFile(classFile)
	if err != nil {
		return false, fmt.Sprintf("cannot read class file: %v", err)
	}
	class := strings.TrimSpace(string(classData))

	// 检查是否为 3D controller 或 VGA controller
	if !strings.HasPrefix(class, PCI3DControllerClass) && !strings.HasPrefix(class, PCIVGAControllerClass) {
		return false, fmt.Sprintf("class %s is not GPU (expected %s or %s)", class, PCI3DControllerClass, PCIVGAControllerClass)
	}

	// 读取 vendor ID
	vendorFile := filepath.Join(devicePath, "vendor")
	vendorData, err := ioutil.ReadFile(vendorFile)
	if err != nil {
		return false, fmt.Sprintf("cannot read vendor file: %v", err)
	}
	vendor := strings.TrimSpace(string(vendorData))

	// 检查是否为 NVIDIA 或 AMD GPU
	switch vendor {
	case NVIDIAVendorID, AMDVendorID:
		return true, fmt.Sprintf("GPU detected: class=%s, vendor=%s", class, vendor)
	default:
		return false, fmt.Sprintf("vendor %s is not NVIDIA or AMD", vendor)
	}
}

// readNUMANode 读取设备的 NUMA 节点
func readNUMANode(devicePath string) (int, error) {
	numaNodeFile := filepath.Join(devicePath, "numa_node")
	data, err := ioutil.ReadFile(numaNodeFile)
	if err != nil {
		return -1, fmt.Errorf("failed to read numa_node: %v", err)
	}

	numaNode, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return -1, fmt.Errorf("failed to parse numa_node: %v", err)
	}

	// -1 表示没有 NUMA 亲和性，回退到 NUMA 0
	if numaNode < 0 {
		klog.Warningf("GPU at %s has numa_node=-1, falling back to NUMA 0", filepath.Base(devicePath))
		return 0, nil
	}

	return numaNode, nil
}

// getGPUDeviceInfo 尝试获取 GPU 的 minor number 和 UUID
// 通过 /proc/driver/nvidia/gpus/<pci_bus_id>/information
func getGPUDeviceInfo(pciBusID string) (minorNumber int, uuid string) {
	// 默认值
	minorNumber = -1
	uuid = ""

	// 使用容器内的挂载路径
	infoPath := filepath.Join("/host/proc/driver/nvidia/gpus", pciBusID, "information")
	data, err := ioutil.ReadFile(infoPath)
	if err != nil {
		// /proc/driver/nvidia 可能不可用，这不是错误
		klog.V(5).Infof("Failed to read GPU info from %s: %v (this is normal if NVIDIA driver is not loaded)", infoPath, err)
		return
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)

		// 解析 "Device Minor: 0"
		if strings.HasPrefix(line, "Device Minor:") {
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				if num, err := strconv.Atoi(fields[2]); err == nil {
					minorNumber = num
				}
			}
		}

		// 解析 "GPU UUID: GPU-abc123..."
		if strings.HasPrefix(line, "GPU UUID:") {
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				uuid = fields[2]
			}
		}
	}

	return
}

// GetResourceInfoMap 返回 GPU 资源信息 (供 CRD 使用)
func (info *GPUNumaInfo) GetResourceInfoMap() v1alpha1.ResourceInfo {
	// 返回 GPU 总数
	gpuCount := len(info.gpuDetail)
	return v1alpha1.ResourceInfo{
		Allocatable: fmt.Sprintf("%d", gpuCount),
		Capacity:    gpuCount,
	}
}

// GetResTopoDetail 返回 GPU 拓扑详细信息
func (info *GPUNumaInfo) GetResTopoDetail() interface{} {
	gpuTopoInfo := make(map[string]v1alpha1.GPUInfo)

	for idx, gpuInfo := range info.gpuDetail {
		gpuTopoInfo[strconv.Itoa(idx)] = gpuInfo
	}

	return gpuTopoInfo
}
