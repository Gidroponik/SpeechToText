package main

import (
	"runtime"
	"sort"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Microphone names come from the MMDevice API (waveIn truncates them to 31
// characters); capture_windows.go maps the endpoint id to a waveIn device.

var (
	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")
	procPropVariantClear = ole32.NewProc("PropVariantClear")
)

var (
	clsidMMDeviceEnumerator = windows.GUID{Data1: 0xBCDE0395, Data2: 0xE52F, Data3: 0x467C, Data4: [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidIMMDeviceEnumerator  = windows.GUID{Data1: 0xA95664D2, Data2: 0x9614, Data3: 0x4F35, Data4: [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	pkeyDeviceFriendlyName  = propertyKey{windows.GUID{Data1: 0xA45C254E, Data2: 0xDF1C, Data3: 0x4EFD, Data4: [8]byte{0x80, 0x20, 0x67, 0xD1, 0x46, 0xA8, 0x50, 0xE0}}, 14}
)

const (
	eRender         = 0
	eCapture        = 1
	eConsole        = 0
	deviceActive    = 1
	clsctxAll       = 0x17
	coinitMulti     = 0x0
	rpcEChangedMode = 0x80010106
	vtLPWSTR        = 31
)

type propertyKey struct {
	fmtid windows.GUID
	pid   uint32
}

// propVariant mirrors PROPVARIANT (24 bytes on amd64).
type propVariant struct {
	vt       uint16
	reserved [6]byte
	val      *uint16
	val2     uintptr
}

type comObject struct{ vtbl *[16]uintptr }

func (o *comObject) call(method int, args ...uintptr) uintptr {
	r, _, _ := syscall.SyscallN(o.vtbl[method], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	return r
}

func (o *comObject) release() {
	if o != nil {
		o.call(2)
	}
}

// Mic is an audio endpoint: a microphone, or an output device for system audio.
type Mic struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
}

// withEnumerator runs fn on a COM-initialised thread with a device enumerator.
func withEnumerator(fn func(enum *comObject)) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		errc <- comThread(fn)
	}()
	return <-errc
}

// comThread initialises COM on the current (locked) thread and runs fn
// with a device enumerator.
func comThread(fn func(enum *comObject)) error {
	hr, _, _ := procCoInitializeEx.Call(0, coinitMulti)
	if int32(hr) >= 0 {
		defer procCoUninitialize.Call()
	} else if uint32(hr) != rpcEChangedMode {
		return syscall.Errno(hr)
	}
	var enum *comObject
	hr, _, _ = procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)), uintptr(unsafe.Pointer(&enum)))
	if int32(hr) < 0 {
		return syscall.Errno(hr)
	}
	defer enum.release()
	fn(enum)
	return nil
}

func takeCoString(p *uint16) string {
	if p == nil {
		return ""
	}
	s := windows.UTF16PtrToString(p)
	procCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
	return s
}

func endpointID(dev *comObject) string {
	var p *uint16
	if int32(dev.call(5, uintptr(unsafe.Pointer(&p)))) < 0 {
		return ""
	}
	return takeCoString(p)
}

func friendlyName(dev *comObject) string {
	var store *comObject
	if int32(dev.call(4, 0, uintptr(unsafe.Pointer(&store)))) < 0 {
		return ""
	}
	defer store.release()
	var pv propVariant
	if int32(store.call(5, uintptr(unsafe.Pointer(&pkeyDeviceFriendlyName)), uintptr(unsafe.Pointer(&pv)))) < 0 {
		return ""
	}
	defer procPropVariantClear.Call(uintptr(unsafe.Pointer(&pv)))
	if pv.vt != vtLPWSTR || pv.val == nil {
		return ""
	}
	return windows.UTF16PtrToString(pv.val)
}

func defaultEndpointOf(enum *comObject, flow uintptr) string {
	var dev *comObject
	if int32(enum.call(4, flow, eConsole, uintptr(unsafe.Pointer(&dev)))) < 0 {
		return ""
	}
	defer dev.release()
	return endpointID(dev)
}

// listEndpoints returns the active endpoints of a flow (eCapture for
// microphones, eRender for speakers and headphones), default first.
func listEndpoints(flow uintptr) ([]Mic, error) {
	var list []Mic
	err := withEnumerator(func(enum *comObject) {
		def := defaultEndpointOf(enum, flow)
		var coll *comObject
		if int32(enum.call(3, flow, deviceActive, uintptr(unsafe.Pointer(&coll)))) < 0 {
			return
		}
		defer coll.release()
		var n uint32
		coll.call(3, uintptr(unsafe.Pointer(&n)))
		for i := uint32(0); i < n; i++ {
			var dev *comObject
			if int32(coll.call(4, uintptr(i), uintptr(unsafe.Pointer(&dev)))) < 0 {
				continue
			}
			m := Mic{ID: endpointID(dev), Name: friendlyName(dev)}
			dev.release()
			if m.ID == "" {
				continue
			}
			m.IsDefault = m.ID == def
			list = append(list, m)
		}
	})
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].IsDefault != list[j].IsDefault {
			return list[i].IsDefault
		}
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
	if list == nil {
		list = []Mic{}
	}
	return list, err
}

func listMics() ([]Mic, error)    { return listEndpoints(eCapture) }
func listOutputs() ([]Mic, error) { return listEndpoints(eRender) }

func defaultMicID() (id string) {
	_ = withEnumerator(func(enum *comObject) { id = defaultEndpointOf(enum, eCapture) })
	return id
}
