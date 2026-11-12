//go:build windows

package main

// GPU temperature, board power and fan speed from the vendor's own driver
// library: AMD's ADL (atiadlxx.dll) or NVIDIA's NVML (nvml.dll). Both are
// installed by the display driver itself, so nothing extra has to run. When
// neither loads, or the adapter answers nothing sensible, the readers return
// -1 (fan: 0).

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

var (
	procLocalAlloc = kernel32.NewProc("LocalAlloc")
	vendorOnce     sync.Once
	vendorTemp     func() int
	vendorPower    func() int
	vendorFan      func() int
)

// vendorInit binds the temperature, power and fan readers once: ADL first,
// NVML when no AMD adapter answered.
func vendorInit() {
	vendorOnce.Do(func() {
		vendorTemp, vendorPower, vendorFan = adlInit()
		if vendorTemp == nil && vendorPower == nil && vendorFan == nil {
			vendorTemp, vendorPower, vendorFan = nvmlInit()
		}
	})
}

// gpuVendorTemp: GPU core/edge temperature in °C from the driver library, or -1.
func gpuVendorTemp() int {
	vendorInit()
	if vendorTemp == nil {
		return -1
	}
	return vendorTemp()
}

// gpuVendorPower: GPU board (package) power in W from the driver library, or -1.
func gpuVendorPower() int {
	vendorInit()
	if vendorPower == nil {
		return -1
	}
	return vendorPower()
}

// gpuVendorFan: GPU fan speed in RPM from the driver library, or 0. NVML only
// reports a percentage, so NVIDIA fans come from LibreHardwareMonitor instead.
func gpuVendorFan() int {
	vendorInit()
	if vendorFan == nil {
		return 0
	}
	if v := vendorFan(); v > 0 {
		return v
	}
	return 0
}

func plausibleWatts(w int) bool { return w > 0 && w < 2000 }
func plausibleRPM(r int) bool   { return r > 0 && r < 10000 }

// systemDLL loads a DLL from System32 only, never from the working directory.
func systemDLL(name string) *syscall.LazyDLL {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return syscall.NewLazyDLL(filepath.Join(root, "System32", name))
}

func plausibleTemp(c int) bool { return c > 0 && c < 150 }

// ---------- AMD ADL --------------------------------------------------------------

const (
	adlAdapterInfoSize = 1572 // sizeof(AdapterInfo): see offsets below
	adlOffBusNumber    = 264
	adlOffVendorID     = 276
	adlOffPresent      = 792
	adlOK              = 0
	adlVendorAMD       = 1002 // iVendorID is decimal 1002 (0x1002 is the PCI ID)

	// ADLSensorType indices in ADLPMLogDataOutput.sensors
	pmlogTempEdge    = 8
	pmlogTempHotspot = 27
	pmlogAsicPower   = 23 // whole-board power, W (what the driver calls "GPU Package")
	pmlogGfxPower    = 30 // graphics core rail only, W
	pmlogFanRPM      = 14
	pmlogSensors     = 256
)

// adlInit returns temperature, power and fan readers bound to the first AMD
// adapter that answers, trying the Navi+ PMLog API, then Overdrive N / 6 / 5
// for temperature, Overdrive 6 for power and Overdrive 5 for the fan. Any
// reader may be nil.
func adlInit() (temp, power, fan func() int) {
	adl := systemDLL("atiadlxx.dll")
	if adl.Load() != nil {
		return nil, nil, nil
	}
	proc := func(n string) *syscall.LazyProc {
		p := adl.NewProc(n)
		if p.Find() != nil {
			return nil
		}
		return p
	}
	create, count, info := proc("ADL2_Main_Control_Create"), proc("ADL2_Adapter_NumberOfAdapters_Get"), proc("ADL2_Adapter_AdapterInfo_Get")
	if create == nil || count == nil || info == nil {
		return nil, nil, nil
	}
	// ADL wants a malloc callback even when we hand it our own buffers.
	cb := syscall.NewCallback(func(size uintptr) uintptr {
		r, _, _ := procLocalAlloc.Call(0x40 /*LPTR*/, size)
		return r
	})
	var ctx uintptr
	if r, _, _ := create.Call(cb, 1, uintptr(unsafe.Pointer(&ctx))); int32(r) != adlOK || ctx == 0 {
		return nil, nil, nil
	}
	var n int32
	if r, _, _ := count.Call(ctx, uintptr(unsafe.Pointer(&n))); int32(r) != adlOK || n <= 0 || n > 256 {
		return nil, nil, nil
	}
	infos := make([]byte, int(n)*adlAdapterInfoSize)
	if r, _, _ := info.Call(ctx, uintptr(unsafe.Pointer(&infos[0])), uintptr(len(infos))); int32(r) != adlOK {
		return nil, nil, nil
	}

	pmlog, odn, od6, od5 := proc("ADL2_New_QueryPMLogData_Get"), proc("ADL2_OverdriveN_Temperature_Get"),
		proc("ADL2_Overdrive6_Temperature_Get"), proc("ADL2_Overdrive5_Temperature_Get")
	od6Power := proc("ADL2_Overdrive6_CurrentPower_Get")
	od5Fan := proc("ADL2_Overdrive5_FanSpeed_Get")

	// pmlogValue: the first of the given sensors that the adapter supports and
	// reports above zero. ADLPMLogDataOutput { int size; struct { int supported, value; } sensors[256]; }
	pmlogValue := func(idx int32, sensors ...int) int {
		if pmlog == nil {
			return -1
		}
		var out [1 + 2*pmlogSensors]int32
		out[0] = int32(unsafe.Sizeof(out))
		if r, _, _ := pmlog.Call(ctx, uintptr(idx), uintptr(unsafe.Pointer(&out[0]))); int32(r) != adlOK {
			return -1
		}
		for _, s := range sensors {
			if out[1+2*s] != 0 && out[2+2*s] > 0 {
				return int(out[2+2*s])
			}
		}
		return -1
	}
	readers := []func(idx int32) int{
		func(idx int32) int { return pmlogValue(idx, pmlogTempEdge, pmlogTempHotspot) },
		func(idx int32) int { // millidegrees, type 1 = core
			if odn == nil {
				return -1
			}
			var milli int32
			if r, _, _ := odn.Call(ctx, uintptr(idx), 1, uintptr(unsafe.Pointer(&milli))); int32(r) != adlOK {
				return -1
			}
			return int(milli / 1000)
		},
		func(idx int32) int {
			if od6 == nil {
				return -1
			}
			var milli int32
			if r, _, _ := od6.Call(ctx, uintptr(idx), uintptr(unsafe.Pointer(&milli))); int32(r) != adlOK {
				return -1
			}
			return int(milli / 1000)
		},
		func(idx int32) int { // ADLTemperature { int iSize; int iTemperature(milli) }, thermal controller 0
			if od5 == nil {
				return -1
			}
			var t [2]int32
			t[0] = 8
			if r, _, _ := od5.Call(ctx, uintptr(idx), 0, uintptr(unsafe.Pointer(&t[0]))); int32(r) != adlOK {
				return -1
			}
			return int(t[1] / 1000)
		},
	}
	powerReaders := []func(idx int32) int{
		func(idx int32) int { return pmlogValue(idx, pmlogAsicPower, pmlogGfxPower) },
		func(idx int32) int { // ADL2_Overdrive6_CurrentPower_Get(ctx, idx, type 0 = total board power, int*): 8.8 fixed-point W
			if od6Power == nil {
				return -1
			}
			var p int32
			if r, _, _ := od6Power.Call(ctx, uintptr(idx), 0, uintptr(unsafe.Pointer(&p))); int32(r) != adlOK {
				return -1
			}
			return int(p >> 8)
		},
	}
	fanReaders := []func(idx int32) int{
		func(idx int32) int { return pmlogValue(idx, pmlogFanRPM) },
		func(idx int32) int { // ADLFanSpeedValue { int iSize, iSpeedType, iFanSpeed, iFlags }; controller 0, type 2 = RPM
			if od5Fan == nil {
				return -1
			}
			var f [4]int32
			f[0], f[1] = 16, 2
			if r, _, _ := od5Fan.Call(ctx, uintptr(idx), 0, uintptr(unsafe.Pointer(&f[0]))); int32(r) != adlOK {
				return -1
			}
			return int(f[2])
		},
	}

	// One AdapterInfo per display output; dedupe physical GPUs by bus number.
	// The first adapter that answers any question provides all the readers.
	seenBus := map[int32]bool{}
	for i := 0; i < int(n); i++ {
		a := infos[i*adlAdapterInfoSize:]
		bus := int32(binary.LittleEndian.Uint32(a[adlOffBusNumber:]))
		if int32(binary.LittleEndian.Uint32(a[adlOffVendorID:])) != adlVendorAMD || binary.LittleEndian.Uint32(a[adlOffPresent:]) == 0 || seenBus[bus] {
			continue
		}
		seenBus[bus] = true
		idx := int32(binary.LittleEndian.Uint32(a[4:])) // iAdapterIndex
		temp = bindReader(readers, idx, plausibleTemp)
		power = bindReader(powerReaders, idx, plausibleWatts)
		fan = bindReader(fanReaders, idx, plausibleRPM)
		if temp != nil || power != nil || fan != nil {
			return temp, power, fan
		}
	}
	return nil, nil, nil
}

// bindReader picks the first reader that currently gives a plausible value
// for the adapter and wraps it so later implausible answers become -1.
func bindReader(readers []func(idx int32) int, idx int32, plausible func(int) bool) func() int {
	for _, rd := range readers {
		if plausible(rd(idx)) {
			return func() int {
				if v := rd(idx); plausible(v) {
					return v
				}
				return -1
			}
		}
	}
	return nil
}

// ---------- NVIDIA NVML ----------------------------------------------------------

func nvmlInit() (temp, power, fan func() int) {
	nvml := systemDLL("nvml.dll")
	if nvml.Load() != nil {
		return nil, nil, nil
	}
	initp, byIndex, getTemp := nvml.NewProc("nvmlInit_v2"), nvml.NewProc("nvmlDeviceGetHandleByIndex_v2"), nvml.NewProc("nvmlDeviceGetTemperature")
	getPower := nvml.NewProc("nvmlDeviceGetPowerUsage")
	if initp.Find() != nil || byIndex.Find() != nil || getTemp.Find() != nil {
		return nil, nil, nil
	}
	if r, _, _ := initp.Call(); r != 0 {
		return nil, nil, nil
	}
	var dev uintptr
	if r, _, _ := byIndex.Call(0, uintptr(unsafe.Pointer(&dev))); r != 0 || dev == 0 {
		return nil, nil, nil
	}
	temp = func() int {
		var t uint32
		const nvmlTemperatureGPU = 0
		if r, _, _ := getTemp.Call(dev, nvmlTemperatureGPU, uintptr(unsafe.Pointer(&t))); r != 0 || !plausibleTemp(int(t)) {
			return -1
		}
		return int(t)
	}
	if getPower.Find() == nil {
		power = func() int { // milliwatts for the whole board
			var mw uint32
			if r, _, _ := getPower.Call(dev, uintptr(unsafe.Pointer(&mw))); r != 0 {
				return -1
			}
			w := int((mw + 500) / 1000)
			if !plausibleWatts(w) {
				return -1
			}
			return w
		}
	}
	return temp, power, nil // nvmlDeviceGetFanSpeed is a percentage, not RPM
}
