package procIO

import (
	"fraunhofer/fkie/yapscan/procIO/customWin32"
	"os"
	"syscall"

	"github.com/0xrawsec/golang-win32/win32"
	"github.com/0xrawsec/golang-win32/win32/kernel32"
)

func GetRunningPIDs() ([]int, error) {
	snap, err := kernel32.CreateToolhelp32Snapshot(kernel32.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}

	pids := make([]int, 0)

	procEntry := kernel32.NewProcessEntry32W()

	_, err = kernel32.Process32FirstW(snap, &procEntry)
	if err != nil && err.(syscall.Errno) != win32.ERROR_NO_MORE_FILES {
		return nil, err
	}
	pids = append(pids, int(procEntry.Th32ProcessID))
	for {
		err = customWin32.Process32NextW(snap, &procEntry)
		if err != nil {
			break
		}
		pids = append(pids, int(procEntry.Th32ProcessID))
	}
	if err.(syscall.Errno) != win32.ERROR_NO_MORE_FILES {
		return nil, err
	}
	return pids, nil
}

type processWindows struct {
	pid        int
	procHandle win32.HANDLE
	suspended  bool
}

func open(pid int) (Process, error) {
	handle, err := kernel32.OpenProcess(
		kernel32.PROCESS_VM_READ|kernel32.PROCESS_QUERY_INFORMATION|kernel32.PROCESS_SUSPEND_RESUME,
		win32.FALSE,
		win32.DWORD(pid),
	)
	if err != nil {
		return nil, err
	}

	return &processWindows{pid: pid, procHandle: handle}, nil
}

func (p *processWindows) PID() int {
	return p.pid
}

func (p *processWindows) Info() *ProcessInfo {
	return &ProcessInfo{
		PID: p.pid,
	}
}

func (p *processWindows) String() string {
	return FormatPID(p.pid)
}

func (p *processWindows) Suspend() error {
	if p.pid == os.Getpid() {
		return ErrProcIsSelf
	}
	if p.pid == os.Getppid() {
		return ErrProcIsParent
	}
	err := customWin32.SuspendProcess(p.pid)
	if err == nil {
		p.suspended = true
	}
	return err
}

func (p *processWindows) Resume() error {
	var err error
	if p.suspended {
		err = customWin32.ResumeProcess(p.pid)
	}
	if err == nil {
		p.suspended = false
	}
	return err
}

func (p *processWindows) Close() error {
	return kernel32.CloseHandle(p.procHandle)
}

func (p *processWindows) Handle() interface{} {
	return p.procHandle
}

func (p *processWindows) MemorySegments() ([]*MemorySegmentInfo, error) {
	segments := make(chan *MemorySegmentInfo)
	errors := make(chan error, 1)

	go func() {
		defer close(segments)
		defer close(errors)

		var currentParent *MemorySegmentInfo

		lpAddress := win32.LPCVOID(0)
		for {
			var mbi win32.MemoryBasicInformation
			mbi, err := kernel32.VirtualQueryEx(p.procHandle, lpAddress)
			if err != nil {
				if err == syscall.Errno(87) {
					// 87 = ERROR_INVALID_PARAMETER is emitted at end of iteration
					err = nil
				}
				errors <- err
				break
			}
			lpAddress += win32.LPCVOID(mbi.RegionSize)
			seg := SegmentFromMemoryBasicInformation(mbi)

			if seg.State == StateFree {
				continue
			}

			if currentParent == nil {
				currentParent = seg
				currentParent.SubSegments = append(currentParent.SubSegments, currentParent.CopyWithoutSubSegments())
			} else {
				if currentParent.ParentBaseAddress == seg.ParentBaseAddress {
					currentParent.SubSegments = append(currentParent.SubSegments, seg)
					currentParent.Size += seg.Size
				} else {
					segments <- currentParent

					currentParent = seg
					currentParent.SubSegments = append(currentParent.SubSegments, currentParent.CopyWithoutSubSegments())
				}
			}
		}
		if currentParent != nil {
			segments <- currentParent
		}
	}()

	segmentsSlice := make([]*MemorySegmentInfo, 0)
	for seg := range segments {
		segmentsSlice = append(segmentsSlice, seg)
	}
	err := <-errors

	return segmentsSlice, err
}
