//go:build windows

package main

// Minimal WMI client over raw COM (ole32/oleaut32 via syscall) so the daemon
// needs no cgo and no third-party module. All calls run on one OS thread that
// owns the COM apartment and caches an IWbemServices per namespace.

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

var (
	ole32                    = syscall.NewLazyDLL("ole32.dll")
	oleaut32                 = syscall.NewLazyDLL("oleaut32.dll")
	procCoInitializeEx       = ole32.NewProc("CoInitializeEx")
	procCoInitializeSecurity = ole32.NewProc("CoInitializeSecurity")
	procCoCreateInstance     = ole32.NewProc("CoCreateInstance")
	procCoSetProxyBlanket    = ole32.NewProc("CoSetProxyBlanket")
	procSysAllocString       = oleaut32.NewProc("SysAllocString")
	procSysFreeString        = oleaut32.NewProc("SysFreeString")
	procVariantClear         = oleaut32.NewProc("VariantClear")
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	clsidWbemLocator = guid{0x4590f811, 0x1d3a, 0x11d0, [8]byte{0x89, 0x1f, 0x00, 0xaa, 0x00, 0x4b, 0x2e, 0x24}}
	iidIWbemLocator  = guid{0xdc12a687, 0x737f, 0x11cf, [8]byte{0x88, 0x4d, 0x00, 0xaa, 0x00, 0x4b, 0x2e, 0x24}}
)

// comObj is a COM interface pointer: a pointer to a vtable. Method i is vtbl[i].
type comObj struct{ vtbl *[32]uintptr }

func (o *comObj) call(i int, args ...uintptr) uintptr {
	r, _, _ := syscall.SyscallN(o.vtbl[i], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	return r
}
func (o *comObj) release() { o.call(2) }

func hrErr(what string, hr uintptr) error {
	if int32(hr) < 0 {
		return fmt.Errorf("%s: HRESULT 0x%08X", what, uint32(hr))
	}
	return nil
}

type bstr uintptr

func newBSTR(s string) bstr {
	p, _ := syscall.UTF16PtrFromString(s)
	r, _, _ := procSysAllocString.Call(uintptr(unsafe.Pointer(p)))
	return bstr(r)
}
func (b bstr) free() { procSysFreeString.Call(uintptr(b)) }

// variant is the 24-byte x64 VARIANT; only the discriminator and the first
// payload word are needed for the scalar types WMI hands back.
type variant struct {
	vt  uint16
	_   [3]uint16
	val uint64
	_   uint64
}

const (
	vtEmpty, vtNull, vtI2, vtI4, vtR4, vtR8, vtBSTR, vtBool = 0, 1, 2, 3, 4, 5, 8, 11
	vtI1, vtUI1, vtUI2, vtUI4, vtI8, vtUI8                  = 16, 17, 18, 19, 20, 21
)

// wmiValue is one property. WMI returns 64-bit integers as strings, so the
// numeric view is also parsed from text.
type wmiValue struct {
	s  string
	n  int64
	ok bool // present and non-null
}

func (v wmiValue) Str() string { return v.s }
func (v wmiValue) Int() int64  { return v.n }

func (v *variant) value() wmiValue {
	switch v.vt {
	case vtEmpty, vtNull:
		return wmiValue{}
	case vtBSTR:
		p := *(**uint16)(unsafe.Pointer(&v.val))
		if p == nil {
			return wmiValue{ok: true}
		}
		s := syscall.UTF16ToString(unsafe.Slice(p, 1<<16)) // stops at the NUL
		t := strings.TrimSpace(s)
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			if f, err := strconv.ParseFloat(t, 64); err == nil {
				n = int64(f)
			}
		}
		return wmiValue{s: s, n: n, ok: true}
	case vtBool:
		if int16(v.val) != 0 {
			return wmiValue{s: "true", n: 1, ok: true}
		}
		return wmiValue{s: "false", ok: true}
	case vtI1:
		return intVal(int64(int8(v.val)))
	case vtI2:
		return intVal(int64(int16(v.val)))
	case vtI4:
		return intVal(int64(int32(v.val)))
	case vtI8:
		return intVal(int64(v.val))
	case vtUI1:
		return intVal(int64(uint8(v.val)))
	case vtUI2:
		return intVal(int64(uint16(v.val)))
	case vtUI4:
		return intVal(int64(uint32(v.val)))
	case vtUI8:
		return intVal(int64(v.val))
	case vtR4:
		f := *(*float32)(unsafe.Pointer(&v.val))
		return wmiValue{s: strconv.FormatFloat(float64(f), 'f', -1, 32), n: int64(f), ok: true}
	case vtR8:
		f := *(*float64)(unsafe.Pointer(&v.val))
		return wmiValue{s: strconv.FormatFloat(f, 'f', -1, 64), n: int64(f), ok: true}
	}
	return wmiValue{s: fmt.Sprintf("vt=%d", v.vt), ok: true}
}

func intVal(n int64) wmiValue { return wmiValue{s: strconv.FormatInt(n, 10), n: n, ok: true} }

// wmiRow maps property name to value for one returned object.
type wmiRow map[string]wmiValue

// ---------- worker thread -----------------------------------------------------

type wmiReq struct {
	ns, query string
	props     []string
	// method call instead of a query when method != "":
	// query holds the object path, in the input parameters.
	method string
	in     map[string]int64
	resp   chan wmiResp
}
type wmiResp struct {
	rows []wmiRow
	err  error
}

var wmiReqs = make(chan wmiReq)

func init() { go wmiWorker() }

// wmiQuery runs a WQL query in namespace ns (e.g. root\CIMV2) and returns the
// named properties of every result. An error means the class or namespace is
// unavailable (unsupported hardware, access denied); callers treat that as
// "no reading", never as fatal.
func wmiQuery(ns, query string, props ...string) ([]wmiRow, error) {
	r := wmiReq{ns: ns, query: query, props: props, resp: make(chan wmiResp, 1)}
	wmiReqs <- r
	x := <-r.resp
	return x.rows, x.err
}

// wmiExecMethod calls method on the object at path (a __RELPATH such as
// `ASUSHW.InstanceName="ACPI\\PNP0C14\\ASUSWMI_0"`) with the given numeric
// input parameters and returns the named output parameters.
func wmiExecMethod(ns, path, method string, in map[string]int64, outProps ...string) (wmiRow, error) {
	r := wmiReq{ns: ns, query: path, method: method, in: in, props: outProps, resp: make(chan wmiResp, 1)}
	wmiReqs <- r
	x := <-r.resp
	if x.err != nil {
		return nil, x.err
	}
	return x.rows[0], nil
}

func wmiWorker() {
	runtime.LockOSThread()
	const coinitMultithreaded = 0
	hr, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded)
	initErr := hrErr("CoInitializeEx", hr)
	if initErr == nil {
		// RPC_C_AUTHN_LEVEL_DEFAULT, RPC_C_IMP_LEVEL_IMPERSONATE; RPC_E_TOO_LATE is harmless.
		procCoInitializeSecurity.Call(0, ^uintptr(0), 0, 0, 0, 3, 0, 0, 0)
	}
	var locator *comObj
	services := map[string]*comObj{}
	for req := range wmiReqs {
		if initErr != nil {
			req.resp <- wmiResp{err: initErr}
			continue
		}
		if locator == nil {
			const clsctxInprocServer = 1
			hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidWbemLocator)), 0, clsctxInprocServer,
				uintptr(unsafe.Pointer(&iidIWbemLocator)), uintptr(unsafe.Pointer(&locator)))
			if err := hrErr("CoCreateInstance(WbemLocator)", hr); err != nil {
				locator = nil
				req.resp <- wmiResp{err: err}
				continue
			}
		}
		key := strings.ToLower(req.ns)
		svc, ok := services[key]
		if !ok {
			var err error
			if svc, err = wmiConnect(locator, req.ns); err != nil {
				req.resp <- wmiResp{err: err}
				continue
			}
			services[key] = svc
		}
		if req.method != "" {
			row, err := wmiMethod(svc, req.query, req.method, req.in, req.props)
			req.resp <- wmiResp{[]wmiRow{row}, err}
			continue
		}
		rows, err := wmiExec(svc, req.query, req.props)
		req.resp <- wmiResp{rows, err}
	}
}

// CIM types as reported by IWbemClassObject::Get, used to shape Put variants.
const (
	cimSint16, cimSint32, cimReal32, cimReal64, cimString, cimBoolean = 2, 3, 4, 5, 8, 11
	cimSint8, cimUint8, cimUint16, cimUint32, cimSint64, cimUint64    = 16, 17, 18, 19, 20, 21
)

// wmiMethod runs IWbemServices::ExecMethod: fetch the class, spawn its input
// parameter object, fill it, call, read the outputs.
func wmiMethod(svc *comObj, path, method string, in map[string]int64, outProps []string) (wmiRow, error) {
	className := path
	if i := strings.IndexAny(className, ".="); i > 0 {
		className = className[:i]
	}
	bClass, bPath, bMethod := newBSTR(className), newBSTR(path), newBSTR(method)
	defer bClass.free()
	defer bPath.free()
	defer bMethod.free()

	var class *comObj
	// IWbemServices::GetObject(path, flags, ctx, &obj, &callResult) is slot 6.
	if hr := svc.call(6, uintptr(bClass), 0, 0, uintptr(unsafe.Pointer(&class)), 0); int32(hr) < 0 {
		return nil, hrErr("GetObject "+className, hr)
	}
	defer class.release()

	var inSig *comObj
	mName, _ := syscall.UTF16PtrFromString(method)
	// IWbemClassObject::GetMethod(name, flags, &inSignature, &outSignature) is slot 19.
	if hr := class.call(19, uintptr(unsafe.Pointer(mName)), 0, uintptr(unsafe.Pointer(&inSig)), 0); int32(hr) < 0 {
		return nil, hrErr("GetMethod "+method, hr)
	}
	var inParams *comObj
	if inSig != nil {
		defer inSig.release()
		// IWbemClassObject::SpawnInstance(flags, &instance) is slot 15.
		if hr := inSig.call(15, 0, uintptr(unsafe.Pointer(&inParams))); int32(hr) < 0 {
			return nil, hrErr("SpawnInstance", hr)
		}
		defer inParams.release()
		for name, val := range in {
			n, _ := syscall.UTF16PtrFromString(name)
			var cur variant
			var cimType int32
			if hr := inParams.call(4, uintptr(unsafe.Pointer(n)), 0, uintptr(unsafe.Pointer(&cur)), uintptr(unsafe.Pointer(&cimType)), 0); int32(hr) < 0 {
				return nil, hrErr("Get in-param "+name, hr)
			}
			procVariantClear.Call(uintptr(unsafe.Pointer(&cur)))
			v := variantFor(cimType, val)
			// IWbemClassObject::Put(name, flags, &variant, type) is slot 5.
			hr := inParams.call(5, uintptr(unsafe.Pointer(n)), 0, uintptr(unsafe.Pointer(&v)), 0)
			procVariantClear.Call(uintptr(unsafe.Pointer(&v)))
			if int32(hr) < 0 {
				return nil, hrErr("Put in-param "+name, hr)
			}
		}
	}

	var outParams *comObj
	var inPtr uintptr
	if inParams != nil {
		inPtr = uintptr(unsafe.Pointer(inParams))
	}
	// IWbemServices::ExecMethod(path, method, flags, ctx, in, &out, &callResult) is slot 24.
	if hr := svc.call(24, uintptr(bPath), uintptr(bMethod), 0, 0, inPtr, uintptr(unsafe.Pointer(&outParams)), 0); int32(hr) < 0 {
		return nil, hrErr("ExecMethod "+method, hr)
	}
	row := wmiRow{}
	if outParams == nil {
		return row, nil
	}
	defer outParams.release()
	for _, p := range append([]string{"ReturnValue"}, outProps...) {
		n, _ := syscall.UTF16PtrFromString(p)
		var v variant
		if hr := outParams.call(4, uintptr(unsafe.Pointer(n)), 0, uintptr(unsafe.Pointer(&v)), 0, 0); int32(hr) >= 0 {
			row[p] = v.value()
			procVariantClear.Call(uintptr(unsafe.Pointer(&v)))
		}
	}
	return row, nil
}

// variantFor builds the VARIANT WMI accepts for a numeric property of the
// given CIM type: VT_UI1 for uint8, VT_BOOL for booleans, VT_BSTR for 64-bit,
// VT_I4 for everything else (WMI does not take VT_UI2/VT_UI4 on Put).
func variantFor(cimType int32, val int64) variant {
	switch cimType {
	case cimUint8:
		return variant{vt: vtUI1, val: uint64(uint8(val))}
	case cimSint8:
		return variant{vt: vtI2, val: uint64(uint16(int16(int8(val))))}
	case cimBoolean:
		if val != 0 {
			return variant{vt: vtBool, val: 0xFFFF}
		}
		return variant{vt: vtBool}
	case cimSint64, cimUint64, cimString:
		return variant{vt: vtBSTR, val: uint64(newBSTR(strconv.FormatInt(val, 10)))}
	}
	return variant{vt: vtI4, val: uint64(uint32(int32(val)))}
}

func wmiConnect(loc *comObj, ns string) (*comObj, error) {
	b := newBSTR(ns)
	defer b.free()
	var svc *comObj
	// IWbemLocator::ConnectServer(ns, user, password, locale, flags, authority, ctx, &svc)
	hr := loc.call(3, uintptr(b), 0, 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&svc)))
	if err := hrErr("ConnectServer "+ns, hr); err != nil {
		return nil, err
	}
	// RPC_C_AUTHN_WINNT, RPC_C_AUTHZ_NONE, principal, RPC_C_AUTHN_LEVEL_CALL, RPC_C_IMP_LEVEL_IMPERSONATE, identity, EOAC_NONE
	procCoSetProxyBlanket.Call(uintptr(unsafe.Pointer(svc)), 10, 0, 0, 3, 3, 0, 0)
	return svc, nil
}

func wmiExec(svc *comObj, query string, props []string) ([]wmiRow, error) {
	lang, q := newBSTR("WQL"), newBSTR(query)
	defer lang.free()
	defer q.free()
	var enum *comObj
	const flags = 0x20 | 0x10 // WBEM_FLAG_FORWARD_ONLY | WBEM_FLAG_RETURN_IMMEDIATELY
	// IWbemServices::ExecQuery is vtable slot 20.
	hr := svc.call(20, uintptr(lang), uintptr(q), flags, 0, uintptr(unsafe.Pointer(&enum)))
	if err := hrErr("ExecQuery", hr); err != nil {
		return nil, err
	}
	defer enum.release()
	names := make([]*uint16, len(props))
	for i, p := range props {
		names[i], _ = syscall.UTF16PtrFromString(p)
	}
	var rows []wmiRow
	for {
		var obj *comObj
		var got uint32
		const wbemInfinite = 0xFFFFFFFF
		// IEnumWbemClassObject::Next(timeout, count, &obj, &returned) is slot 4.
		hr := enum.call(4, wbemInfinite, 1, uintptr(unsafe.Pointer(&obj)), uintptr(unsafe.Pointer(&got)))
		if int32(hr) < 0 {
			return rows, hrErr("IEnumWbemClassObject::Next", hr)
		}
		if got == 0 || obj == nil {
			return rows, nil
		}
		row := wmiRow{}
		for i, p := range props {
			var v variant
			// IWbemClassObject::Get(name, flags, &variant, type, flavor) is slot 4.
			if hr := obj.call(4, uintptr(unsafe.Pointer(names[i])), 0, uintptr(unsafe.Pointer(&v)), 0, 0); int32(hr) >= 0 {
				row[p] = v.value()
				procVariantClear.Call(uintptr(unsafe.Pointer(&v)))
			}
		}
		obj.release()
		rows = append(rows, row)
	}
}
