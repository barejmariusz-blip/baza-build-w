//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

var mainHwnd uintptr

type browseInfo struct {
	Owner        uintptr
	Root         uintptr
	DisplayName  *uint16
	Title        *uint16
	Flags        uint32
	Callback     uintptr
	LParam       uintptr
	Image        int32
}

// pickFolder shows the native Windows "choose folder" dialog. Returns "" when cancelled.
func pickFolder(title string) string {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ole := syscall.NewLazyDLL("ole32.dll")
	ole.NewProc("CoInitializeEx").Call(0, 0x2) // COINIT_APARTMENTTHREADED
	defer ole.NewProc("CoUninitialize").Call()
	shell := syscall.NewLazyDLL("shell32.dll")
	buf := make([]uint16, 260)
	t, _ := syscall.UTF16PtrFromString(title)
	bi := browseInfo{Owner: mainHwnd, DisplayName: &buf[0], Title: t, Flags: 0x0001 | 0x0040} // RETURNONLYFSDIRS | NEWDIALOGSTYLE
	pidl, _, _ := shell.NewProc("SHBrowseForFolderW").Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return ""
	}
	defer ole.NewProc("CoTaskMemFree").Call(pidl)
	path := make([]uint16, 32768)
	r, _, _ := shell.NewProc("SHGetPathFromIDListEx").Call(pidl, uintptr(unsafe.Pointer(&path[0])), uintptr(len(path)), 0)
	if r == 0 {
		return ""
	}
	return syscall.UTF16ToString(path)
}

func configPath() string {
	d, err := os.UserConfigDir() // %APPDATA%
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "BazaBuildow", "config.json")
}

type config struct {
	DataDir string `json:"dataDir"`
}

func loadConfig() config {
	var c config
	if b, err := os.ReadFile(configPath()); err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

func saveConfig(c config) error {
	p := configPath()
	os.MkdirAll(filepath.Dir(p), 0755)
	b, _ := json.MarshalIndent(c, "", "  ")
	return atomicWrite(p, b)
}

func defaultDataDir() string { return filepath.Join(documentsDir(), "BazaBuildow") }

func setDataDir(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "kopie"), 0755); err != nil {
		return err
	}
	// check that the folder is writable
	probe := filepath.Join(dir, ".zapis-test")
	if err := os.WriteFile(probe, []byte("ok"), 0644); err != nil {
		return errors.New("brak prawa zapisu w tym folderze")
	}
	os.Remove(probe)
	os.MkdirAll(filepath.Join(dir, "obrazy"), 0755)
	dataDir = dir
	imgDir = filepath.Join(dir, "obrazy")
	dataFile = filepath.Join(dir, "buildy.json")
	backupDir = filepath.Join(dir, "kopie")
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst + ".tmp")
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	out.Close()
	return os.Rename(dst+".tmp", dst)
}

// moveData copies the library and backups from the current folder into dir (old files stay as they were).
func copyDataTo(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "kopie"), 0755); err != nil {
		return err
	}
	if _, err := os.Stat(dataFile); err == nil {
		if err := copyFile(dataFile, filepath.Join(dir, "buildy.json")); err != nil {
			return err
		}
	}
	os.MkdirAll(filepath.Join(dir, "obrazy"), 0755)
	imgs, _ := filepath.Glob(filepath.Join(imgDir, "*"))
	for _, f := range imgs {
		dst := filepath.Join(dir, "obrazy", filepath.Base(f))
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			if err := copyFile(f, dst); err != nil {
				return err
			}
		}
	}
	files, _ := filepath.Glob(filepath.Join(backupDir, "*.json"))
	for _, f := range files {
		dst := filepath.Join(dir, "kopie", filepath.Base(f))
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			copyFile(f, dst)
		}
	}
	return nil
}
