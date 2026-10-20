//go:build windows

package edge

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type _ICoreWebView2CallDevToolsProtocolMethodCompletedHandlerVtbl struct {
	_IUnknownVtbl
	Invoke ComProc
}

type ICoreWebView2CallDevToolsProtocolMethodCompletedHandler struct {
	vtbl *_ICoreWebView2CallDevToolsProtocolMethodCompletedHandlerVtbl
	impl _ICoreWebView2CallDevToolsProtocolMethodCompletedHandlerImpl
}

func (i *ICoreWebView2CallDevToolsProtocolMethodCompletedHandler) AddRef() uintptr {
	ret, _, _ := i.vtbl.AddRef.Call(uintptr(unsafe.Pointer(i)))
	return ret
}

func _ICoreWebView2CallDevToolsProtocolMethodCompletedHandlerIUnknownQueryInterface(this *ICoreWebView2CallDevToolsProtocolMethodCompletedHandler, refiid, object uintptr) uintptr {
	return this.impl.QueryInterface(refiid, object)
}

func _ICoreWebView2CallDevToolsProtocolMethodCompletedHandlerIUnknownAddRef(this *ICoreWebView2CallDevToolsProtocolMethodCompletedHandler) uintptr {
	return this.impl.AddRef()
}

func _ICoreWebView2CallDevToolsProtocolMethodCompletedHandlerIUnknownRelease(this *ICoreWebView2CallDevToolsProtocolMethodCompletedHandler) uintptr {
	return this.impl.Release()
}

// Invoke(errorCode HRESULT, returnObjectAsJson LPCWSTR)
func _ICoreWebView2CallDevToolsProtocolMethodCompletedHandlerInvoke(this *ICoreWebView2CallDevToolsProtocolMethodCompletedHandler, errorCode uintptr, result uintptr) uintptr {
	name := ""
	if result != 0 {
		for p := (*uint16)(unsafe.Pointer(result)); *p != 0; p = (*uint16)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + 2)) {
			name += string(rune(*p))
		}
	}
	return this.impl.CallDevToolsProtocolMethodCompleted(errorCode, name)
}

type _ICoreWebView2CallDevToolsProtocolMethodCompletedHandlerImpl interface {
	_IUnknownImpl
	CallDevToolsProtocolMethodCompleted(errorCode uintptr, result string) uintptr
}

var _ICoreWebView2CallDevToolsProtocolMethodCompletedHandlerFn = _ICoreWebView2CallDevToolsProtocolMethodCompletedHandlerVtbl{
	_IUnknownVtbl{
		NewComProc(_ICoreWebView2CallDevToolsProtocolMethodCompletedHandlerIUnknownQueryInterface),
		NewComProc(_ICoreWebView2CallDevToolsProtocolMethodCompletedHandlerIUnknownAddRef),
		NewComProc(_ICoreWebView2CallDevToolsProtocolMethodCompletedHandlerIUnknownRelease),
	},
	NewComProc(_ICoreWebView2CallDevToolsProtocolMethodCompletedHandlerInvoke),
}

func newICoreWebView2CallDevToolsProtocolMethodCompletedHandler(impl _ICoreWebView2CallDevToolsProtocolMethodCompletedHandlerImpl) *ICoreWebView2CallDevToolsProtocolMethodCompletedHandler {
	return &ICoreWebView2CallDevToolsProtocolMethodCompletedHandler{
		vtbl: &_ICoreWebView2CallDevToolsProtocolMethodCompletedHandlerFn,
		impl: impl,
	}
}

// CallDevToolsProtocolMethod issues an async CDP call against the webview.
// The result (JSON string) is delivered to cb on the browser's callback.
func (e *Chromium) CallDevToolsProtocol(method, paramsJson string, cb func(errCode int, result string)) error {
	if e.webview == nil {
		return syscall.Errno(0x80004003)
	}
	impl := &cdpHandlerImpl{cb: cb}
	h := newICoreWebView2CallDevToolsProtocolMethodCompletedHandler(impl)
	e.cdpHandler = h // keep alive: COM holds a raw pointer to it
	m16, err := windows.UTF16PtrFromString(method)
	if err != nil {
		return err
	}
	p16, err := windows.UTF16PtrFromString(paramsJson)
	if err != nil {
		return err
	}
	hr, _, _ := e.webview.vtbl.CallDevToolsProtocolMethod.Call(
		uintptr(unsafe.Pointer(e.webview)),
		uintptr(unsafe.Pointer(m16)),
		uintptr(unsafe.Pointer(p16)),
		uintptr(unsafe.Pointer(h)),
	)
	if hr != 0 {
		return syscall.Errno(hr)
	}
	return nil
}

type cdpHandlerImpl struct {
	cb func(errCode int, result string)
}

func (c *cdpHandlerImpl) QueryInterface(refiid, object uintptr) uintptr {
	return uintptr(windows.E_NOINTERFACE)
}

func (c *cdpHandlerImpl) AddRef() uintptr { return 1 }

func (c *cdpHandlerImpl) Release() uintptr { return 1 }

func (c *cdpHandlerImpl) CallDevToolsProtocolMethodCompleted(errorCode uintptr, result string) uintptr {
	if c.cb != nil {
		c.cb(int(int32(errorCode)), result)
	}
	return 0
}
