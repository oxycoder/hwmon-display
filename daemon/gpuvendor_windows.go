//go:build windows

package main

// GPU temperature from the vendor's own driver library: AMD's ADL
// (atiadlxx.dll) or NVIDIA's NVML (nvml.dll). Both are installed by the
// display driver itself, so nothing extra has to run. When neither loads,
// or the adapter answers nothing sensible, gpuVendorTemp returns -1.

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
)

// gpuVendorTemp: GPU core/edge temperature in °C from the driver library, or -1.
func gpuVendorTemp() int {
	vendorOnce.Do(func() {
		vendorTemp = adlInit()
		if vendorTemp == nil {
			vendorTemp = nvmlInit()
		}
	})
	if vendorTemp == nil {
		return -1
	}
	return vendorTemp()
}

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
	pmlogSensors     = 256
)

// adlInit returns a reader bound to the first AMD adapter that reports a
// temperature, trying the Navi+ PMLog API, then Overdrive N / 6 / 5.
func adlInit() func() int {
	adl := systemDLL("atiadlxx.dll")
	if adl.Load() != nil {
		return nil
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
		return nil
	}
	// ADL wants a malloc callback even when we hand it our own buffers.
	cb := syscall.NewCallback(func(size uintptr) uintptr {
		r, _, _ := procLocalAlloc.Call(0x40 /*LPTR*/, size)
		return r
	})
	var ctx uintptr
	if r, _, _ := create.Call(cb, 1, uintptr(unsafe.Pointer(&ctx))); int32(r) != adlOK || ctx == 0 {
		return nil
	}
	var n int32
	if r, _, _ := count.Call(ctx, uintptr(unsafe.Pointer(&n))); int32(r) != adlOK || n <= 0 || n > 256 {
		return nil
	}
	infos := make([]byte, int(n)*adlAdapterInfoSize)
	if r, _, _ := info.Call(ctx, uintptr(unsafe.Pointer(&infos[0])), uintptr(len(infos))); int32(r) != adlOK {
		return nil
	}

	pmlog, odn, od6, od5 := proc("ADL2_New_QueryPMLogData_Get"), proc("ADL2_OverdriveN_Temperature_Get"),
		proc("ADL2_Overdrive6_Temperature_Get"), proc("ADL2_Overdrive5_Temperature_Get")
	readers := []func(idx int32) int{
		func(idx int32) int { // ADLPMLogDataOutput { int size; struct { int supported, value; } sensors[256]; }
			if pmlog == nil {
				return -1
			}
			var out [1 + 2*pmlogSensors]int32
			out[0] = int32(unsafe.Sizeof(out))
			if r, _, _ := pmlog.Call(ctx, uintptr(idx), uintptr(unsafe.Pointer(&out[0]))); int32(r) != adlOK {
				return -1
			}
			for _, s := range []int{pmlogTempEdge, pmlogTempHotspot} {
				if out[1+2*s] != 0 && plausibleTemp(int(out[2+2*s])) {
					return int(out[2+2*s])
				}
			}
			return -1
		},
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

	// One AdapterInfo per display output; dedupe physical GPUs by bus number.
	seenBus := map[int32]bool{}
	for i := 0; i < int(n); i++ {
		a := infos[i*adlAdapterInfoSize:]
		bus := int32(binary.LittleEndian.Uint32(a[adlOffBusNumber:]))
		if int32(binary.LittleEndian.Uint32(a[adlOffVendorID:])) != adlVendorAMD || binary.LittleEndian.Uint32(a[adlOffPresent:]) == 0 || seenBus[bus] {
			continue
		}
		seenBus[bus] = true
		idx := int32(binary.LittleEndian.Uint32(a[4:])) // iAdapterIndex
		for _, rd := range readers {
			if plausibleTemp(rd(idx)) {
				return func() int {
					if c := rd(idx); plausibleTemp(c) {
						return c
					}
					return -1
				}
			}
		}
	}
	return nil
}

// ---------- NVIDIA NVML ----------------------------------------------------------

func nvmlInit() func() int {
	nvml := systemDLL("nvml.dll")
	if nvml.Load() != nil {
		return nil
	}
	initp, byIndex, getTemp := nvml.NewProc("nvmlInit_v2"), nvml.NewProc("nvmlDeviceGetHandleByIndex_v2"), nvml.NewProc("nvmlDeviceGetTemperature")
	if initp.Find() != nil || byIndex.Find() != nil || getTemp.Find() != nil {
		return nil
	}
	if r, _, _ := initp.Call(); r != 0 {
		return nil
	}
	var dev uintptr
	if r, _, _ := byIndex.Call(0, uintptr(unsafe.Pointer(&dev))); r != 0 || dev == 0 {
		return nil
	}
	return func() int {
		var t uint32
		const nvmlTemperatureGPU = 0
		if r, _, _ := getTemp.Call(dev, nvmlTemperatureGPU, uintptr(unsafe.Pointer(&t))); r != 0 || !plausibleTemp(int(t)) {
			return -1
		}
		return int(t)
	}
}
