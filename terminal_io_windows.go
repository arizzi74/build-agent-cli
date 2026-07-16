//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strconv"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

var errWindowsTerminalWouldBlock = errors.New("terminal read would block")

var (
	kernel32ConsoleInput = windows.NewLazySystemDLL("kernel32.dll")
	peekConsoleInputW    = kernel32ConsoleInput.NewProc("PeekConsoleInputW")
	readConsoleInputW    = kernel32ConsoleInput.NewProc("ReadConsoleInputW")
	readConsoleW         = kernel32ConsoleInput.NewProc("ReadConsoleW")
)

type windowsConsoleReader struct {
	sync.Mutex
	pending       []byte
	highSurrogate uint16
}

var windowsConsoleReaders = struct {
	sync.Mutex
	byFD map[int]*windowsConsoleReader
}{byFD: make(map[int]*windowsConsoleReader)}

const (
	windowsKeyEventRecord  = 0x0001
	windowsInputRecordSize = 20

	windowsRightAltPressed  = 0x0001
	windowsLeftAltPressed   = 0x0002
	windowsRightCtrlPressed = 0x0004
	windowsLeftCtrlPressed  = 0x0008
	windowsShiftPressed     = 0x0010
)

func readTerminalFD(fd int, buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	handle := windows.Handle(uintptr(fd))
	var mode uint32
	isConsole := windows.GetConsoleMode(handle, &mode) == nil
	if isConsole {
		baseMode := (mode | windows.ENABLE_EXTENDED_FLAGS) &^ (windows.ENABLE_MOUSE_INPUT | windows.ENABLE_WINDOW_INPUT | windows.ENABLE_QUICK_EDIT_MODE)
		virtualMode := baseMode | windows.ENABLE_VIRTUAL_TERMINAL_INPUT
		if virtualMode == mode || windows.SetConsoleMode(handle, virtualMode) == nil {
			return readWindowsConsoleVT(fd, handle, buffer)
		}
		// ENABLE_VIRTUAL_TERMINAL_INPUT is unavailable on older console hosts.
		// Retain Unicode/navigation support there through direct input-record
		// translation, while modern Windows Terminal/ConPTY gets full VT input
		// including bracketed-paste delimiters.
		if baseMode != mode {
			if err := windows.SetConsoleMode(handle, baseMode); err != nil {
				return 0, err
			}
		}
		return readWindowsConsoleRecords(fd, handle, buffer)
	}
	ready, err := windowsTerminalHandleReady(handle)
	if err != nil {
		return 0, err
	}
	if !ready {
		return 0, errWindowsTerminalWouldBlock
	}
	var n uint32
	if err := windows.ReadFile(handle, buffer, &n, nil); err != nil {
		return int(n), err
	}
	return int(n), nil
}

func windowsConsoleReaderForFD(fd int) *windowsConsoleReader {
	windowsConsoleReaders.Lock()
	defer windowsConsoleReaders.Unlock()
	reader := windowsConsoleReaders.byFD[fd]
	if reader == nil {
		reader = &windowsConsoleReader{}
		windowsConsoleReaders.byFD[fd] = reader
	}
	return reader
}

func readWindowsConsoleVT(fd int, handle windows.Handle, buffer []byte) (int, error) {
	reader := windowsConsoleReaderForFD(fd)
	reader.Lock()
	defer reader.Unlock()
	if n := reader.copyPending(buffer); n > 0 {
		return n, nil
	}

	for {
		ready, err := windowsConsoleVTReady(handle)
		if err != nil {
			return 0, err
		}
		if !ready {
			return 0, errWindowsTerminalWouldBlock
		}
		var units [64]uint16
		var count uint32
		ok, _, callErr := readConsoleW.Call(
			uintptr(handle),
			uintptr(unsafe.Pointer(&units[0])),
			uintptr(len(units)),
			uintptr(unsafe.Pointer(&count)),
			0,
		)
		if ok == 0 {
			return 0, callErr
		}
		if count == 0 {
			return 0, errWindowsTerminalWouldBlock
		}
		for _, unit := range units[:count] {
			reader.pending = append(reader.pending, reader.encodeUTF16(unit)...)
		}
		if n := reader.copyPending(buffer); n > 0 {
			return n, nil
		}
	}
}

func readWindowsConsoleRecords(fd int, handle windows.Handle, buffer []byte) (int, error) {
	reader := windowsConsoleReaderForFD(fd)

	reader.Lock()
	defer reader.Unlock()
	if n := reader.copyPending(buffer); n > 0 {
		return n, nil
	}

	for {
		ready, err := windowsTerminalHandleReady(handle)
		if err != nil {
			return 0, err
		}
		if !ready {
			return 0, errWindowsTerminalWouldBlock
		}

		var record [windowsInputRecordSize]byte
		var count uint32
		ok, _, callErr := readConsoleInputW.Call(
			uintptr(handle),
			uintptr(unsafe.Pointer(&record[0])),
			1,
			uintptr(unsafe.Pointer(&count)),
		)
		if ok == 0 {
			return 0, callErr
		}
		if count == 0 {
			return 0, errWindowsTerminalWouldBlock
		}
		translated := reader.translateKeyRecord(record)
		if len(translated) == 0 {
			continue
		}
		reader.pending = append(reader.pending[:0], translated...)
		n := copy(buffer, reader.pending)
		reader.pending = reader.pending[n:]
		return n, nil
	}
}

func (reader *windowsConsoleReader) copyPending(buffer []byte) int {
	if len(reader.pending) == 0 {
		return 0
	}
	n := copy(buffer, reader.pending)
	reader.pending = reader.pending[n:]
	return n
}

func windowsConsoleVTReady(handle windows.Handle) (bool, error) {
	for {
		ready, err := windowsTerminalHandleReady(handle)
		if err != nil || !ready {
			return ready, err
		}
		var record [windowsInputRecordSize]byte
		var count uint32
		ok, _, callErr := peekConsoleInputW.Call(
			uintptr(handle),
			uintptr(unsafe.Pointer(&record[0])),
			1,
			uintptr(unsafe.Pointer(&count)),
		)
		if ok == 0 {
			return false, callErr
		}
		if count == 0 {
			return false, nil
		}
		if binary.LittleEndian.Uint16(record[0:2]) == windowsKeyEventRecord && binary.LittleEndian.Uint32(record[4:8]) != 0 {
			virtualKey := binary.LittleEndian.Uint16(record[10:12])
			unicodeChar := binary.LittleEndian.Uint16(record[14:16])
			if unicodeChar != 0 || windowsVirtualTerminalKey(virtualKey) {
				return true, nil
			}
		}
		count = 0
		ok, _, callErr = readConsoleInputW.Call(
			uintptr(handle),
			uintptr(unsafe.Pointer(&record[0])),
			1,
			uintptr(unsafe.Pointer(&count)),
		)
		if ok == 0 {
			return false, callErr
		}
	}
}

func windowsVirtualTerminalKey(virtualKey uint16) bool {
	switch virtualKey {
	case 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x2e: // End, Home, arrows, Delete.
		return true
	default:
		return false
	}
}

func windowsTerminalHandleReady(handle windows.Handle) (bool, error) {
	event, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return false, err
	}
	if event == uint32(windows.WAIT_TIMEOUT) {
		return false, nil
	}
	if event != windows.WAIT_OBJECT_0 {
		return false, errors.New("unexpected terminal wait result")
	}
	return true, nil
}

func (reader *windowsConsoleReader) translateKeyRecord(record [windowsInputRecordSize]byte) []byte {
	if binary.LittleEndian.Uint16(record[0:2]) != windowsKeyEventRecord || binary.LittleEndian.Uint32(record[4:8]) == 0 {
		return nil
	}
	repeat := int(binary.LittleEndian.Uint16(record[8:10]))
	if repeat <= 0 {
		repeat = 1
	}
	virtualKey := binary.LittleEndian.Uint16(record[10:12])
	unicodeChar := binary.LittleEndian.Uint16(record[14:16])
	controlState := binary.LittleEndian.Uint32(record[16:20])
	if unicodeChar == 0 {
		return bytes.Repeat(windowsNavigationSequence(virtualKey, controlState), repeat)
	}
	shift := controlState&windowsShiftPressed != 0
	if unicodeChar == '\r' && shift {
		return bytes.Repeat([]byte("\x1b[13;2u"), repeat)
	}
	encoded := reader.encodeUTF16(unicodeChar)
	if len(encoded) == 0 {
		return nil
	}
	alt := controlState&(windowsLeftAltPressed|windowsRightAltPressed) != 0
	ctrl := controlState&(windowsLeftCtrlPressed|windowsRightCtrlPressed) != 0
	altGr := controlState&windowsRightAltPressed != 0 && ctrl
	if alt && !altGr {
		encoded = append([]byte{0x1b}, encoded...)
	}
	return bytes.Repeat(encoded, repeat)
}

func (reader *windowsConsoleReader) encodeUTF16(unit uint16) []byte {
	var out []byte
	if reader.highSurrogate != 0 {
		high := reader.highSurrogate
		reader.highSurrogate = 0
		if unit >= 0xdc00 && unit <= 0xdfff {
			return []byte(string(utf16.DecodeRune(rune(high), rune(unit))))
		}
		out = append(out, string(utf8.RuneError)...)
	}
	if unit >= 0xd800 && unit <= 0xdbff {
		reader.highSurrogate = unit
		return out
	}
	if unit >= 0xdc00 && unit <= 0xdfff {
		return append(out, string(utf8.RuneError)...)
	}
	return append(out, string(rune(unit))...)
}

func windowsNavigationSequence(virtualKey uint16, controlState uint32) []byte {
	var final byte
	switch virtualKey {
	case 0x23: // End
		final = 'F'
	case 0x24: // Home
		final = 'H'
	case 0x25: // Left
		final = 'D'
	case 0x26: // Up
		final = 'A'
	case 0x27: // Right
		final = 'C'
	case 0x28: // Down
		final = 'B'
	case 0x2e: // Delete
		return []byte("\x1b[3~")
	default:
		return nil
	}
	modifier := 1
	if controlState&windowsShiftPressed != 0 {
		modifier++
	}
	if controlState&(windowsLeftAltPressed|windowsRightAltPressed) != 0 {
		modifier += 2
	}
	if controlState&(windowsLeftCtrlPressed|windowsRightCtrlPressed) != 0 {
		modifier += 4
	}
	if modifier == 1 {
		return []byte{'\x1b', '[', final}
	}
	return []byte("\x1b[1;" + strconv.Itoa(modifier) + string(final))
}

func terminalReadWouldBlock(err error) bool {
	return errors.Is(err, errWindowsTerminalWouldBlock)
}

func startTerminalResizeNotifications(redraw func()) func() {
	width, height, err := term.GetSize(terminalStderrFD())
	if err != nil {
		width, height = 0, 0
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				newWidth, newHeight, sizeErr := term.GetSize(terminalStderrFD())
				if sizeErr == nil && (newWidth != width || newHeight != height) {
					width, height = newWidth, newHeight
					redraw()
				}
			}
		}
	}()
	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			close(done)
			<-stopped
		})
	}
}
