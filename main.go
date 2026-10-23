//go:build windows

package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

//go:embed ui/index.html
var indexHTML []byte

//go:embed seed.json
var seedJSON []byte

const appName = "Baza Buildów"
const keepBackups = 60

var (
	dataDir   string
	dataFile  string
	backupDir string
	imgDir    string
	mu        sync.Mutex
)

func msgBox(text string) {
	u := syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW")
	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString(appName)
	u.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), 0x10)
}

func documentsDir() string {
	if p, err := windows.KnownFolderPath(windows.FOLDERID_Documents, 0); err == nil && p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "Documents")
}

func atomicWrite(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// One automatic copy per day (overwritten during the day), keep the newest N.
func dailyBackup(b []byte) {
	name := "buildy-" + time.Now().Format("2006-01-02") + ".json"
	_ = atomicWrite(filepath.Join(backupDir, name), b)
	files, _ := filepath.Glob(filepath.Join(backupDir, "buildy-*.json"))
	sort.Strings(files)
	for len(files) > keepBackups {
		os.Remove(files[0])
		files = files[1:]
	}
}

type library struct {
	Games  json.RawMessage `json:"games"`
	Builds json.RawMessage `json:"builds"`
}

func wrap(l library) []byte {
	payload := map[string]any{
		"app":        "Baza Buildów",
		"version":    1,
		"exportedAt": time.Now().UTC().Format(time.RFC3339),
		"games":      l.Games,
		"builds":     l.Builds,
	}
	b, _ := json.MarshalIndent(payload, "", "  ")
	return b
}

func loadData() (library, error) {
	var l library
	b, err := os.ReadFile(dataFile)
	if os.IsNotExist(err) {
		if err := atomicWrite(dataFile, seedJSON); err != nil {
			return l, err
		}
		b = seedJSON
	} else if err != nil {
		return l, err
	}
	if json.Unmarshal(b, &l) != nil || l.Games == nil {
		return l, fmt.Errorf("plik danych jest uszkodzony: %s", dataFile)
	}
	if l.Builds == nil {
		l.Builds = json.RawMessage("[]")
	}
	return l, nil
}

func openExternal(target string) {
	exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
}

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

var titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
var ogTitleRe = regexp.MustCompile(`(?is)<meta[^>]+property=["']og:title["'][^>]+content=["']([^"']+)["']`)
var safeImg = regexp.MustCompile(`^[a-z0-9-]+\.(png|jpg|jpeg|webp|gif)$`)

func fetchTitle(u string) string {
	c := &http.Client{Timeout: 7 * time.Second}
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36")
	req.Header.Set("Accept-Language", "pl,en;q=0.8")
	resp, err := c.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if m := ogTitleRe.FindSubmatch(b); m != nil {
		return strings.TrimSpace(html.UnescapeString(string(m[1])))
	}
	if m := titleRe.FindSubmatch(b); m != nil {
		return strings.Join(strings.Fields(html.UnescapeString(string(m[1]))), " ")
	}
	return ""
}

func serve(ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(indexHTML)
	})
	mux.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			l, err := loadData()
			if err != nil {
				w.WriteHeader(500)
				jsonOut(w, map[string]string{"error": err.Error()})
				return
			}
			jsonOut(w, map[string]any{"games": l.Games, "builds": l.Builds, "file": dataFile, "dir": dataDir, "defaultDir": defaultDataDir(), "isDefault": filepath.Clean(dataDir) == filepath.Clean(defaultDataDir())})
		case http.MethodPost:
			body, err := io.ReadAll(io.LimitReader(r.Body, 100<<20))
			var l library
			if err == nil {
				err = json.Unmarshal(body, &l)
			}
			if err != nil || l.Games == nil || l.Builds == nil {
				w.WriteHeader(400)
				jsonOut(w, map[string]string{"error": "nieprawidłowe dane"})
				return
			}
			b := wrap(l)
			if err := atomicWrite(dataFile, b); err != nil {
				w.WriteHeader(500)
				jsonOut(w, map[string]string{"error": err.Error()})
				return
			}
			dailyBackup(b)
			jsonOut(w, map[string]any{"ok": true, "savedAt": time.Now().Format("15:04:05")})
		}
	})
	// Screenshots: body = data URL, saved as a file in <folder>\obrazy
	mux.HandleFunc("/api/image", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 40<<20))
		s := string(body)
		ext := "png"
		if i := strings.Index(s, ","); i > 0 && strings.HasPrefix(s, "data:image/") {
			head := s[:i]
			switch {
			case strings.Contains(head, "jpeg"), strings.Contains(head, "jpg"):
				ext = "jpg"
			case strings.Contains(head, "webp"):
				ext = "webp"
			case strings.Contains(head, "gif"):
				ext = "gif"
			}
			s = s[i+1:]
		}
		raw, err2 := base64.StdEncoding.DecodeString(s)
		if err != nil || err2 != nil || len(raw) == 0 {
			w.WriteHeader(400)
			jsonOut(w, map[string]string{"error": "nieprawidłowy obraz"})
			return
		}
		name := fmt.Sprintf("img-%d.%s", time.Now().UnixNano(), ext)
		mu.Lock()
		err = atomicWrite(filepath.Join(imgDir, name), raw)
		mu.Unlock()
		if err != nil {
			w.WriteHeader(500)
			jsonOut(w, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, map[string]string{"name": name})
	})
	mux.HandleFunc("/img/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/img/")
		if !safeImg.MatchString(name) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "max-age=31536000")
		http.ServeFile(w, r, filepath.Join(imgDir, name))
	})
	mux.HandleFunc("/api/title", func(w http.ResponseWriter, r *http.Request) {
		u := r.URL.Query().Get("u")
		t := ""
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			t = fetchTitle(u)
		}
		jsonOut(w, map[string]string{"title": t})
	})
	mux.HandleFunc("/api/backup", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		b, err := os.ReadFile(dataFile)
		if err == nil {
			name := "reczna-" + time.Now().Format("2006-01-02_15-04-05") + ".json"
			err = atomicWrite(filepath.Join(backupDir, name), b)
			if err == nil {
				jsonOut(w, map[string]string{"file": filepath.Join(backupDir, name)})
				return
			}
		}
		w.WriteHeader(500)
		jsonOut(w, map[string]string{"error": err.Error()})
	})
	mux.HandleFunc("/api/pick-folder", func(w http.ResponseWriter, r *http.Request) {
		p := pickFolder("Wybierz folder, w którym Baza Buildów ma zapisywać dane, screeny i kopie")
		if p == "" {
			jsonOut(w, map[string]any{"cancelled": true})
			return
		}
		_, err := os.Stat(filepath.Join(p, "buildy.json"))
		jsonOut(w, map[string]any{"path": p, "hasLibrary": err == nil, "same": filepath.Clean(p) == filepath.Clean(dataDir)})
	})
	// mode: "copy" = przenieś obecną bazę do nowego folderu, "use" = wczytaj bazę, która już tam jest
	mux.HandleFunc("/api/set-folder", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var req struct{ Path, Mode string }
		json.NewDecoder(r.Body).Decode(&req)
		if req.Path == "default" {
			req.Path = defaultDataDir()
		}
		fail := func(err error) {
			w.WriteHeader(500)
			jsonOut(w, map[string]string{"error": err.Error()})
		}
		if req.Path == "" {
			fail(fmt.Errorf("nie podano folderu"))
			return
		}
		if req.Mode != "use" {
			if err := copyDataTo(req.Path); err != nil {
				fail(err)
				return
			}
		}
		if err := setDataDir(req.Path); err != nil {
			fail(err)
			return
		}
		cfg := config{DataDir: req.Path}
		if filepath.Clean(req.Path) == filepath.Clean(defaultDataDir()) {
			cfg.DataDir = ""
		}
		if err := saveConfig(cfg); err != nil {
			fail(err)
			return
		}
		jsonOut(w, map[string]any{"ok": true, "dir": dataDir})
	})
	mux.HandleFunc("/api/open-folder", func(w http.ResponseWriter, r *http.Request) {
		exec.Command("explorer", dataDir).Start()
		jsonOut(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/open-url", func(w http.ResponseWriter, r *http.Request) {
		u := r.URL.Query().Get("u")
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			openExternal(u)
		}
		jsonOut(w, map[string]bool{"ok": true})
	})
	http.Serve(ln, mux)
}

func main() {
	dir := loadConfig().DataDir
	if dir == "" {
		dir = defaultDataDir()
	}
	if err := setDataDir(dir); err != nil {
		msgBox("Nie mogę użyć folderu danych:\n" + dir + "\n\n" + err.Error() + "\n\nWracam do domyślnego folderu w Dokumentach.")
		if err := setDataDir(defaultDataDir()); err != nil {
			msgBox("Nie mogę utworzyć folderu danych:\n" + defaultDataDir() + "\n\n" + err.Error())
			return
		}
		saveConfig(config{})
	}

	ln, err := net.Listen("tcp", "127.0.0.1:47631")
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			msgBox("Nie mogę uruchomić lokalnego serwera: " + err.Error())
			return
		}
	}
	go serve(ln)

	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		local = dataDir
	}
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  filepath.Join(local, "BazaBuildow", "webview"),
		WindowOptions: webview2.WindowOptions{
			Title:  "Baza Buildów",
			Width:  1360,
			Height: 900,
			IconId: 1,
			Center: true,
		},
	})
	if w == nil {
		msgBox("Brakuje komponentu Microsoft Edge WebView2.\nZainstaluj go ze strony Microsoft (WebView2 Runtime) i uruchom program ponownie.")
		openExternal("https://developer.microsoft.com/microsoft-edge/webview2/")
		return
	}
	defer w.Destroy()
	mainHwnd = uintptr(w.Window())
	w.Navigate(fmt.Sprintf("http://%s/", ln.Addr().String()))
	w.Run()
}
