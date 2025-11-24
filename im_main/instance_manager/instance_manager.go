package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"container/heap"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
)

type Server struct {
	ID          int
	Port        int
	Cmd         *exec.Cmd
	Status      string
	cleanupOnce sync.Once
}

var (
	servers    = make(map[int]*Server) // protected by serversMux
	serversMux sync.Mutex
	serverMap  = make(map[string]*Server) // name -> *Server, protected by mu
	mu         sync.Mutex
	nextPort   = 3000       // start from whatever base you want
	available  = &IntHeap{} // min-heap of freed ports
)

const (
	proxyApiHost    = "http://172.30.0.1:8081"
	defaultFallback = "lobby"
	token           = "" // NOTE: Hardcoded token
	repoWorlds      = "JuMaEn16/lunexia-worlds"
)

type IntHeap []int

func (h IntHeap) Len() int            { return len(h) }
func (h IntHeap) Less(i, j int) bool  { return h[i] < h[j] } // min-heap
func (h IntHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *IntHeap) Push(x interface{}) { *h = append(*h, x.(int)) }
func (h *IntHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

type Instance struct {
	Name        string   `json:"name"`
	Players     []string `json:"players"`
	PlayerCount int64    `json:"player_count"`
	TPS         int8     `json:"tps"`
	Port        int      `json:"port"`
	Status      string   `json:"status"`
}

type SystemInfo struct {
	CPUPercent float64    `json:"cpu_percent,omitempty"`
	RAMUsedMB  uint64     `json:"ram_used_mb,omitempty"`
	RAMTotalMB uint64     `json:"ram_total_mb,omitempty"`
	Instances  []Instance `json:"instances,omitempty"`
}

func systemHandler(w http.ResponseWriter, r *http.Request) {
	// Get CPU percentage
	cpuPercent, err := cpu.Percent(time.Second, false)
	if err != nil {
		http.Error(w, fmt.Sprintf("CPU error: %v", err), http.StatusInternalServerError)
		return
	}

	// Get memory statistics
	vmStat, err := mem.VirtualMemory()
	if err != nil {
		http.Error(w, fmt.Sprintf("Memory error: %v", err), http.StatusInternalServerError)
		return
	}

	// Track running servers by name and include their ports and status
	mu.Lock()
	var instances []Instance
	for name, s := range serverMap {
		var (
			port   int
			status string
		)

		// If 's' is not nil, the server instance exists.
		// If 's' is nil, it might be an entry for a server that is stopped.
		if s != nil {
			port = int(s.Port)
			// We get the status directly from the server struct
			status = s.Status // <-- READ STATUS
		} else {
			// If s is nil, we can consider it "stopped"
			status = "running"
		}

		instances = append(instances, Instance{
			Name:   name,
			Port:   port,
			Status: status, // <-- ASSIGN STATUS
		})
	}
	mu.Unlock()

	// Create the final response struct
	sysInfo := SystemInfo{
		CPUPercent: cpuPercent[0], // cpu.Percent returns a slice, take the first element
		RAMUsedMB:  vmStat.Used / 1024 / 1024,
		RAMTotalMB: vmStat.Total / 1024 / 1024,
		Instances:  instances,
	}

	// Encode and send the JSON response
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(sysInfo); err != nil {
		// Log error if encoding fails
		fmt.Printf("Error encoding JSON response: %v\n", err)
	}
}

func setupServerDir(dir string, port int, name string) error {
	// Create server directory
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// Accept EULA
	if err := os.WriteFile(filepath.Join(dir, "eula.txt"), []byte("eula=true\n"), 0644); err != nil {
		return err
	}

	// Write updated server.properties
	props := fmt.Sprintf(
		`server-port=%d
motd=Dynamic Paper Server %d
enable-command-block=true
online-mode=false
`, port, port)

	if err := os.WriteFile(filepath.Join(dir, "server.properties"), []byte(props), 0644); err != nil {
		return err
	}

	// Create config directory
	configDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return err
	}

	// Write paper-global.yaml
	paperGlobal := `proxies:
  bungee-cord:
    online-mode: true
  proxy-protocol: false
  velocity:
    enabled: true
    online-mode: true
    secret: qJQe07fSMCfn
`
	if err := os.WriteFile(filepath.Join(configDir, "paper-global.yml"), []byte(paperGlobal), 0644); err != nil {
		return err
	}

	// Write ops.json
	ops := `
[
    {
        "uuid": "ff642073-9c37-4956-8404-7d10fabaf254",
        "name": "JuMaEn16",
        "level": 4,
        "bypassesPlayerLimit": false
    },
	{
		"uuid": "d60196b5-b291-41d0-913e-20e19bf502fb",
        "name": "einMitsuki",
        "level": 4,
        "bypassesPlayerLimit": false
	}
]`
	if err := os.WriteFile(filepath.Join(dir, "ops.json"), []byte(ops), 0644); err != nil {
		return err
	}

	// Copy paper.jar into the new server directory
	jarSrc := "paper.jar"
	jarDst := filepath.Join(dir, "paper.jar")

	if err := copyFile(jarSrc, jarDst); err != nil {
		return fmt.Errorf("failed copying paper.jar: %w", err)
	}

	// If you now have a local "plugins" folder, copy it entirely into the server dir.
	localPluginsFolder := "plugins" // <-- local plugins folder you prepared
	serverPluginsFolder := filepath.Join(dir, "plugins")

	// Make sure destination plugins folder exists; copyDir will create it anyway.
	if err := copyDir(localPluginsFolder, serverPluginsFolder); err != nil {
		return fmt.Errorf("failed copying plugins folder: %w", err)
	}

	// Write LunexiaMain config (plugin folder)
	configLunexia := fmt.Sprintf(`type: "%s"`, name)

	lunexiaDst := filepath.Join(serverPluginsFolder, "LunexiaMain")
	if err := os.MkdirAll(lunexiaDst, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(lunexiaDst, "config.yml"), []byte(configLunexia), 0644); err != nil {
		return err
	}

	// --- WORLD DOWNLOAD HERE ---
	worldURL := fmt.Sprintf("https://raw.githubusercontent.com/JuMaEn16/lunexia-worlds/main/%s.zip", name)

	result := make(chan error)
	DownloadWorldAsync(worldURL, token, dir, result)

	fmt.Println("[World] Waiting for download + extraction...")
	if err := <-result; err != nil {
		return fmt.Errorf("world install failed: %w", err)
	}

	fmt.Println("[World] Ready!")
	return nil
}

func copyDir(src string, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read dir %s: %w", src, err)
	}

	// create destination root
	if err := os.MkdirAll(dst, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", srcPath, err)
		}

		if info.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			in, err := os.Open(srcPath)
			if err != nil {
				return fmt.Errorf("open %s: %w", srcPath, err)
			}
			// ensure file is closed promptly
			func() {
				defer in.Close()
				out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
				if err != nil {
					err = fmt.Errorf("create %s: %w", dstPath, err)
					// reassign to outer err for return
					panic(err)
				}
				defer out.Close()

				if _, err := io.Copy(out, in); err != nil {
					err = fmt.Errorf("copy %s -> %s: %w", srcPath, dstPath, err)
					panic(err)
				}
			}()
			// handle panic used for early-return inside closure
			if r := recover(); r != nil {
				if e, ok := r.(error); ok {
					return e
				}
				return fmt.Errorf("unknown error copying file %s", srcPath)
			}
		}
	}
	return nil
}

func DownloadWorldAsync(
	url string,
	token string,
	destDir string,
	result chan<- error,
) {
	go func() {
		zipPath := filepath.Join(destDir, "world.zip")

		fmt.Println("[World] Starting world download...")

		// STEP 1: Download ZIP with progress
		if err := downloadWithProgress(url, zipPath, token); err != nil {
			result <- fmt.Errorf("download failed: %w", err)
			return
		}

		// STEP 3: Delete existing world directory
		worldDir := filepath.Join(destDir, "world")
		if _, err := os.Stat(worldDir); err == nil {
			fmt.Println("[World] Removing old world...")
			if err := os.RemoveAll(worldDir); err != nil {
				result <- fmt.Errorf("failed to delete old world: %w", err)
				return
			}
		}

		// STEP 4: Extract new world
		fmt.Println("[World] Extracting world...")
		if err := unzip(zipPath, worldDir); err != nil {
			result <- fmt.Errorf("extract failed: %w", err)
			return
		}

		fmt.Println("[World] World successfully installed!")
		result <- nil
	}()
}

func downloadWithProgress(url, dest, token string) error {
	client := &http.Client{}

	req, _ := http.NewRequest("GET", url, nil)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	total := resp.ContentLength
	var downloaded int64
	buf := make([]byte, 32*1024)

	start := time.Now()
	lastPrint := time.Now()

	fmt.Println("[World] Downloading...")

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, wErr := out.Write(buf[:n])
			if wErr != nil {
				return wErr
			}
			downloaded += int64(n)
		}

		if time.Since(lastPrint) >= time.Second {
			percent := float64(downloaded) / float64(total) * 100
			speed := float64(downloaded) / time.Since(start).Seconds() / 1024 / 1024
			fmt.Printf("[World] %.1f%% (%.2f MB/s)\n", percent, speed)
			lastPrint = time.Now()
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	return nil
}

func unzip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		fpath := filepath.Join(dest, f.Name)

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(fpath, 0755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(fpath), 0755); err != nil {
			return err
		}

		in, err := f.Open()
		if err != nil {
			return err
		}
		defer in.Close()

		out, err := os.OpenFile(fpath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}
		defer out.Close()

		if _, err := io.Copy(out, in); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	if err != nil {
		return err
	}

	return out.Sync()
}

func allocatePort() int {
	serversMux.Lock()
	defer serversMux.Unlock()

	if available.Len() > 0 {
		p := heap.Pop(available).(int)
		return p
	}
	p := nextPort
	nextPort++
	return p
}

func releasePort(p int) {
	// ignore invalid ports <= 0
	if p <= 0 {
		return
	}
	serversMux.Lock()
	defer serversMux.Unlock()
	heap.Push(available, p)
}

// ---------- startServerHandler (waits for "Done" and uses lowest port) ----------
func startServerHandler(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "Missing 'name' query parameter", http.StatusBadRequest)
		return
	}

	// ensure only one server per name
	mu.Lock()
	if _, exists := serverMap[name]; exists {
		mu.Unlock()
		http.Error(w, "Server already running", http.StatusBadRequest)
		return
	}
	mu.Unlock()

	// get lowest available port (from heap or nextPort)
	port := allocatePort()
	// if anything fails before registration, return the port to pool
	allocatedAndPending := true
	defer func() {
		if allocatedAndPending {
			// something failed; make port available again
			releasePort(port)
		}
	}()

	dir := fmt.Sprintf("paper_server_%d", port)
	if err := setupServerDir(dir, port, name); err != nil {
		http.Error(w, "Failed to set up server directory: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// build command
	cmd := exec.Command(
		"java",
		"-Xmx2G", "-Xms2G",
		"-jar", "paper.jar",
		"--nogui",
	)
	cmd.Dir = dir

	// capture output so we can wait for "Done"
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, "Failed to create stdout pipe: "+err.Error(), http.StatusInternalServerError)
		return
	}
	cmd.Stderr = cmd.Stdout

	// start
	if err := cmd.Start(); err != nil {
		http.Error(w, "Failed to start server: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// monitor output for the "Done" line
	started := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Println(line) // still emit to host console

			// match typical Paper/Bukkit done message
			if strings.Contains(line, "Done") && strings.Contains(line, "For help") {
				close(started)
				return
			}
		}
		// if scanner ends without "Done", close channel (caller will timeout)
		close(started)
	}()

	// wait for "Done" or timeout
	select {
	case <-started:
		// either success (Done found) or scanner closed — we still check by probing server process state below
	case <-time.After(60 * time.Second):
		// timeout: kill process and return error
		_ = cmd.Process.Kill()
		http.Error(w, "Server start timed out", http.StatusGatewayTimeout)
		return
	}

	// At this point server has produced lines and likely started. Register it.
	srv := &Server{ID: port, Port: port, Cmd: cmd, Status: "running"}

	serversMux.Lock()
	servers[srv.ID] = srv
	serversMux.Unlock()

	mu.Lock()
	serverMap[name] = srv
	mu.Unlock()

	// mark allocation as completed — don't put the port back in the pool
	allocatedAndPending = false

	fmt.Printf("Paper server '%s' fully started on port %d\n", name, srv.Port)
	w.Write([]byte(fmt.Sprintf("Server '%s' started on port %d", name, srv.Port)))
}

// /// Example stop handler that returns the freed port to the pool /////
func stopServerHandler(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "Missing 'name' query parameter", http.StatusBadRequest)
		return
	}

	// find server by name and remove mapping
	mu.Lock()
	srv, exists := serverMap[name]
	if !exists {
		mu.Unlock()
		http.Error(w, fmt.Sprintf("Server '%s' not found", name), http.StatusNotFound)
		return
	}
	delete(serverMap, name)
	mu.Unlock()

	// stop actual server by ID and remove from servers map
	serversMux.Lock()
	realSrv, ok := servers[srv.ID]
	if !ok {
		serversMux.Unlock()
		http.Error(w, fmt.Sprintf("Server '%s' exists but internal server ID %d not found", name, srv.ID), http.StatusInternalServerError)
		return
	}

	// graceful stop
	if err := realSrv.Cmd.Process.Signal(syscall.SIGINT); err != nil {
		serversMux.Unlock()
		http.Error(w, "Failed to stop server: "+err.Error(), http.StatusInternalServerError)
		return
	}

	delete(servers, realSrv.ID)
	serversMux.Unlock()

	// return the port to the pool so it becomes the lowest available next time
	releasePort(realSrv.ID)

	fmt.Printf("Stopped server '%s' (ID %d)\n", name, realSrv.ID)
	w.Write([]byte(fmt.Sprintf("Server '%s' stopped", name)))
}

func stopServerHold(name string, srv *Server) error {
	cmdPtr := srv.Cmd
	srvID := srv.ID

	// 1. Check if process is valid
	if cmdPtr == nil || cmdPtr.Process == nil {
		log.Printf("Server '%s' process not available (already stopped?)", name)
		// Ensure status is "stopped" (nil)
		mu.Lock()
		serverMap[name] = nil
		mu.Unlock()
		return fmt.Errorf("server process not available (already stopped?)")
	}

	// 2. Signal SIGINT then wait with timeout
	if err := cmdPtr.Process.Signal(syscall.SIGINT); err != nil {
		return fmt.Errorf("failed to signal server: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmdPtr.Wait()
	}()

	select {
	case err := <-waitCh:
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			// Log but continue
			log.Printf("server process wait for '%s' returned err (continuing): %v", name, err)
		}
	case <-time.After(30 * time.Second):
		// didn't exit in time — force kill
		log.Printf("Server '%s' did not stop in 30s, killing...", name)
		_ = cmdPtr.Process.Kill()
		<-waitCh // wait for Wait() to return
	}

	log.Printf("Server '%s' process stopped.", name)

	// 3. Remove process references from maps
	serversMux.Lock()
	delete(servers, srvID)
	serversMux.Unlock()

	mu.Lock()
	serverMap[name] = nil // <-- STATUS UPDATE 2 (nil = "stopped" in systemHandler)
	log.Printf("Server '%s' set to nil in serverMap (stopped).", name)
	mu.Unlock()

	return nil
}

func startHeldServer(name string, port int, dir string) error {
	// 1. Set status to "restarting"
	// Create a new Server object for the new process, including the cleanupOnce guard
	srv := &Server{ID: port, Port: port, Status: "restarting", Cmd: nil}
	mu.Lock()
	serverMap[name] = srv // <-- STATUS UPDATE 3
	log.Printf("Server '%s' status set to 'restarting'", name)
	mu.Unlock()

	pluginSrc := "LunexiaMain.jar"
	pluginDst := filepath.Join(filepath.Join(dir, "plugins"), "LunexiaMain.jar")

	if err := copyFile(pluginSrc, pluginDst); err != nil {
		return fmt.Errorf("failed copying LunexiaMain.jar: %w", err)
	}

	// 2. start server again (same port/dir/name)
	cmd := exec.Command(
		"java",
		"-Xmx2G", "-Xms2G",
		"-jar", "paper.jar",
		"--nogui",
	)
	cmd.Dir = dir
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("Failed to create stdout pipe for restart: %v", err)
		// On error, set status back to stopped
		mu.Lock()
		serverMap[name] = nil
		mu.Unlock()
		return fmt.Errorf("failed to create stdout pipe for restart: %v", err)
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		log.Printf("Failed to restart server: %v", err)
		// On error, set status back to stopped
		mu.Lock()
		serverMap[name] = nil
		mu.Unlock()
		return fmt.Errorf("failed to restart server: %v", err)
	}

	// Update the live server object with the Cmd reference
	srv.Cmd = cmd

	// 3. monitor "Done" similar to start-server
	started := make(chan struct{})

	// Function to safely close the 'started' channel once
	safeCloseStarted := func() {
		srv.cleanupOnce.Do(func() {
			close(started)
			log.Printf("Channel 'started' safely closed for '%s'.", name)
		})
	}

	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Println(line) // keep console output
			if strings.Contains(line, "Done") && strings.Contains(line, "For help") {
				safeCloseStarted() // Use safe closure 1
				// Don't return here, let the pipe drain
			}
		}
		log.Printf("Stdout pipe closed for '%s'", name)
		safeCloseStarted() // Use safe closure 2 (will only run if 1 hasn't yet)
	}()

	// 4. Wait for start or timeout
	select {
	case <-started:
		log.Printf("Server '%s' restart detected 'Done' line.", name)
		// Success, update the server maps
		mu.Lock()
		// Update the server object status
		if currentSrv, ok := serverMap[name]; ok && currentSrv != nil {
			currentSrv.Status = "running" // <-- STATUS UPDATE 4
			log.Printf("Server '%s' status set to 'running'", name)

			// Add to the 'servers' map as well
			serversMux.Lock()
			servers[currentSrv.ID] = currentSrv
			serversMux.Unlock()
		} else {
			log.Printf("Error: serverMap entry for '%s' was nil or missing after restart", name)
		}
		mu.Unlock()
		return nil // Success

	case <-time.After(60 * time.Second):
		log.Printf("Server '%s' restart timed out after 60s.", name)
		_ = cmd.Process.Kill()
		// Set status back to "stopped"
		mu.Lock()
		serverMap[name] = nil
		mu.Unlock()

		return fmt.Errorf("server restart timed out")
	}
}

func saveWorldHandler(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "Missing 'name' query parameter", http.StatusBadRequest)
		return
	}

	// HTTP client used for proxy calls
	client := &http.Client{Timeout: 5 * time.Second}

	// --- A. Evacuate players: call proxy /move_from_to BEFORE stopping the server ---
	var movedPlayers []string
	{
		proxyUrl, err := url.Parse(proxyApiHost + "/move_from_to")
		if err != nil {
			log.Printf("CRITICAL: Failed to parse proxyApiHost URL: %v", err)
			http.Error(w, "Internal configuration error: invalid proxy host", http.StatusInternalServerError)
			return
		}

		q := url.Values{}
		q.Add("origin", name)
		// If we have a configured fallback and it's not the same server, ask to move players there.
		if defaultFallback != "" && defaultFallback != name {
			q.Add("destination", defaultFallback)
		}
		proxyUrl.RawQuery = q.Encode()

		log.Printf("Requesting proxy to move players AWAY from '%s' (proxy endpoint: %s)", name, proxyUrl.String())
		resp, err := client.Get(proxyUrl.String())
		if err != nil {
			log.Printf("ERROR: Failed to call proxy /move_from_to for '%s': %v", name, err)
			http.Error(w, fmt.Sprintf("Failed to contact proxy to move players away: %v", err), http.StatusInternalServerError)
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("ERROR: Proxy returned non-OK status (%d) for /move_from_to: %s", resp.StatusCode, string(body))
			http.Error(w, fmt.Sprintf("Proxy error during /move_from_to (%d): %s", resp.StatusCode, string(body)), http.StatusInternalServerError)
			return
		}

		// Parse JSON response to extract moved_players
		var mvResp struct {
			Ok           bool     `json:"ok"`
			OriginServer string   `json:"origin_server"`
			DestServer   string   `json:"dest_server"`
			MovedPlayers []string `json:"moved_players"`
		}
		if err := json.Unmarshal(body, &mvResp); err != nil {
			log.Printf("ERROR: Failed to parse /move_from_to response JSON: %v (body: %s)", err, string(body))
			http.Error(w, fmt.Sprintf("Invalid proxy response during /move_from_to: %v", err), http.StatusInternalServerError)
			return
		}

		if !mvResp.Ok {
			log.Printf("ERROR: Proxy reported ok=false for /move_from_to: %s", string(body))
			http.Error(w, fmt.Sprintf("Proxy reported failure during /move_from_to: %s", string(body)), http.StatusInternalServerError)
			return
		}

		// store moved players (may be empty)
		movedPlayers = mvResp.MovedPlayers
		log.Printf("Proxy moved players away from '%s': %v", name, movedPlayers)
	}

	// --- B. locate server and set status to "restarting" ---
	mu.Lock()
	srv, exists := serverMap[name]
	if !exists {
		mu.Unlock()
		http.Error(w, fmt.Sprintf("Server '%s' not found", name), http.StatusNotFound)
		return
	}
	if srv == nil {
		mu.Unlock()
		http.Error(w, fmt.Sprintf("Server '%s' is already stopped. Use 'start' instead.", name), http.StatusBadRequest)
		return
	}

	// Mark as restarting
	srv.Status = "restarting"
	log.Printf("Server '%s' status set to 'restarting'", name)

	// copy needed fields and release lock
	port := srv.Port
	mu.Unlock()

	// compute server dir (same convention used when starting)
	dir := fmt.Sprintf("paper_server_%d", port)
	worldDir := filepath.Join(dir, "world")
	if _, err := os.Stat(worldDir); os.IsNotExist(err) {
		http.Error(w, fmt.Sprintf("World directory does not exist: %s", worldDir), http.StatusBadRequest)
		return
	}

	// --- Stop Server Gracefully ---
	// We call the new function. It handles its own locking.
	if err := stopServerHold(name, srv); err != nil {
		// stopServerHold already logged the details and updated maps if needed
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// --- Server is now stopped and de-registered ---

	// create zip file (temporary)
	tmpZip, err := os.CreateTemp("", fmt.Sprintf("%s-*.zip", name))
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create temp zip: %v", err), http.StatusInternalServerError)
		return
	}
	zipPath := tmpZip.Name()
	tmpZip.Close()
	defer os.Remove(zipPath)

	log.Printf("Zipping world for '%s'...", name)
	if err := zipDir(worldDir, zipPath, []string{"advancements", "playerdata", "stats"}); err != nil {
		http.Error(w, fmt.Sprintf("Failed to zip world: %v", err), http.StatusInternalServerError)
		return
	}
	// "owner/repo"
	if token == "" || repoWorlds == "" {
		http.Error(w, "GitHub token/repo not set", http.StatusInternalServerError)
		return
	}

	// destination path in repo: {name}.zip
	destPath := path.Base(fmt.Sprintf("%s.zip", name))

	log.Printf("Uploading world for '%s' to GitHub...", name)
	if err := uploadFileToGitHub(zipPath, repoWorlds, destPath, token, fmt.Sprintf("Save world %s at %s", name, time.Now().UTC().Format(time.RFC3339))); err != nil {
		http.Error(w, fmt.Sprintf("Failed to upload to GitHub: %v", err), http.StatusInternalServerError)
		return
	}
	log.Printf("Upload complete for '%s'.", name)

	// --- Server Restart ---
	// Call the new startHeldServer function
	if err := startHeldServer(name, port, dir); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// --- C. After successful restart: ask proxy to move players BACK to this server ---
	if len(movedPlayers) > 0 {
		// Small sleep to give the restarted server a moment to accept connections
		time.Sleep(1 * time.Second)

		proxyUrl, err := url.Parse(proxyApiHost + "/move_list_to")
		if err != nil {
			log.Printf("CRITICAL: Failed to parse proxyApiHost URL for /move_list: %v", err)
		} else {
			q := url.Values{}
			// join player list with commas (API expected format: players=name1,name2,...)
			q.Add("players", strings.Join(movedPlayers, ","))
			q.Add("server", name)
			proxyUrl.RawQuery = q.Encode()

			log.Printf("Requesting proxy to move players BACK to '%s' (proxy endpoint: %s)", name, proxyUrl.String())
			resp, err := client.Get(proxyUrl.String())
			if err != nil {
				// Do NOT fail the restart because of a proxy notification error; only log it.
				log.Printf("ERROR: Failed to call proxy /move_list for '%s': %v", name, err)
			} else {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					log.Printf("ERROR: Proxy returned non-OK status (%d) for /move_list: %s", resp.StatusCode, string(body))
				} else {
					log.Printf("Proxy /move_list response: %s", string(body))
				}
			}
		}
	} else {
		log.Printf("No players were moved away from '%s' earlier; skipping /move_list.", name)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf("World saved to GitHub as %s and server restarted on port %d", destPath, port)))
}

// zipDir zips all files inside srcDir into destZip (file path)
func zipDir(srcDir, destZip string, blacklist []string) error {
	zipFile, err := os.Create(destZip)
	if err != nil {
		return err
	}
	defer zipFile.Close()

	w := zip.NewWriter(zipFile)
	defer w.Close()

	blacklistMap := make(map[string]struct{})
	for _, name := range blacklist {
		blacklistMap[name] = struct{}{}
	}

	return filepath.Walk(srcDir, func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(srcDir, file)
		if err != nil {
			return err
		}

		// skip root
		if relPath == "." {
			return nil
		}

		// Check blacklist: if any path segment matches, skip
		parts := strings.Split(relPath, string(os.PathSeparator))
		for _, p := range parts {
			if _, ok := blacklistMap[p]; ok {
				if fi.IsDir() {
					return filepath.SkipDir // skip entire directory
				}
				return nil // skip file
			}
		}

		// directories
		if fi.IsDir() {
			_, err := w.Create(relPath + "/")
			return err
		}

		// files
		fh, err := zip.FileInfoHeader(fi)
		if err != nil {
			return err
		}
		fh.Name = relPath
		fh.Method = zip.Deflate

		writer, err := w.CreateHeader(fh)
		if err != nil {
			return err
		}

		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = io.Copy(writer, f)
		return err
	})
}

func uploadFileToGitHub(localPath, repo, destPath, token, message string) error {
	// read local file
	content, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	b64 := base64.StdEncoding.EncodeToString(content)

	// parse repo into owner/repo
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("GITHUB_REPO must be in owner/repo format")
	}
	owner := parts[0]
	reponame := parts[1]

	client := &http.Client{Timeout: 30 * time.Second}

	// check if file exists to get its sha (for updates)
	getURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s",
		url.PathEscape(owner), url.PathEscape(reponame), url.PathEscape(destPath))
	getReq, _ := http.NewRequest("GET", getURL, nil)
	getReq.Header.Set("Authorization", "token "+token)
	getReq.Header.Set("Accept", "application/vnd.github+json")

	getResp, err := client.Do(getReq)
	if err != nil {
		return fmt.Errorf("failed to query existing file: %w", err)
	}
	// read body then close
	bodyBytes, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()

	var sha string
	if getResp.StatusCode == http.StatusOK {
		// file exists -> extract sha
		var info struct {
			SHA string `json:"sha"`
		}
		if err := json.Unmarshal(bodyBytes, &info); err != nil {
			return fmt.Errorf("failed to parse existing file info: %w", err)
		}
		if info.SHA == "" {
			return fmt.Errorf("existing file returned no sha")
		}
		sha = info.SHA
	} else if getResp.StatusCode == http.StatusNotFound {
		// file does not exist -> will create (sha stays empty)
		sha = ""
	} else {
		// other error (rate limit, permissions, etc.)
		// include body for easier debugging
		return fmt.Errorf("GitHub GET contents returned status %d: %s", getResp.StatusCode, string(bodyBytes))
	}

	// prepare request body for create/update
	reqBody := map[string]interface{}{
		"message": message,
		"content": b64,
		"branch":  "main",
	}
	if sha != "" {
		reqBody["sha"] = sha
	}
	jsonBody, _ := json.Marshal(reqBody)

	putURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s",
		owner, reponame, destPath)
	putReq, _ := http.NewRequest("PUT", putURL, bytes.NewReader(jsonBody))
	putReq.Header.Set("Authorization", "token "+token)
	putReq.Header.Set("Accept", "application/vnd.github+json")
	putReq.Header.Set("Content-Type", "application/json")

	putResp, err := client.Do(putReq)
	if err != nil {
		return fmt.Errorf("GitHub PUT request failed: %w", err)
	}
	defer putResp.Body.Close()

	respBody, _ := io.ReadAll(putResp.Body)
	if putResp.StatusCode != http.StatusCreated && putResp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API error: status %d: %s", putResp.StatusCode, string(respBody))
	}

	return nil
}

func restartWorldHandler(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "Missing 'name' query parameter", http.StatusBadRequest)
		return
	}

	// HTTP client used for proxy calls
	client := &http.Client{Timeout: 5 * time.Second}

	// --- A. Evacuate players: call proxy /move_from_to BEFORE stopping the server ---
	var movedPlayers []string
	{
		proxyUrl, err := url.Parse(proxyApiHost + "/move_from_to")
		if err != nil {
			log.Printf("CRITICAL: Failed to parse proxyApiHost URL: %v", err)
			http.Error(w, "Internal configuration error: invalid proxy host", http.StatusInternalServerError)
			return
		}

		q := url.Values{}
		q.Add("reason", "Server is restarting..")
		q.Add("origin", name)
		// If we have a configured fallback and it's not the same server, ask to move players there.
		if defaultFallback != "" && defaultFallback != name {
			q.Add("destination", defaultFallback)
		}
		proxyUrl.RawQuery = q.Encode()

		log.Printf("Requesting proxy to move players AWAY from '%s' (proxy endpoint: %s)", name, proxyUrl.String())
		resp, err := client.Get(proxyUrl.String())
		if err != nil {
			log.Printf("ERROR: Failed to call proxy /move_from_to for '%s': %v", name, err)
			http.Error(w, fmt.Sprintf("Failed to contact proxy to move players away: %v", err), http.StatusInternalServerError)
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("ERROR: Proxy returned non-OK status (%d) for /move_from_to: %s", resp.StatusCode, string(body))
			http.Error(w, fmt.Sprintf("Proxy error during /move_from_to (%d): %s", resp.StatusCode, string(body)), http.StatusInternalServerError)
			return
		}

		// Parse JSON response to extract moved_players
		var mvResp struct {
			Ok           bool     `json:"ok"`
			OriginServer string   `json:"origin_server"`
			DestServer   string   `json:"dest_server"`
			MovedPlayers []string `json:"moved_players"`
		}
		if err := json.Unmarshal(body, &mvResp); err != nil {
			log.Printf("ERROR: Failed to parse /move_from_to response JSON: %v (body: %s)", err, string(body))
			http.Error(w, fmt.Sprintf("Invalid proxy response during /move_from_to: %v", err), http.StatusInternalServerError)
			return
		}

		if !mvResp.Ok {
			log.Printf("ERROR: Proxy reported ok=false for /move_from_to: %s", string(body))
			http.Error(w, fmt.Sprintf("Proxy reported failure during /move_from_to: %s", string(body)), http.StatusInternalServerError)
			return
		}

		// store moved players (may be empty)
		movedPlayers = mvResp.MovedPlayers
		log.Printf("Proxy moved players away from '%s': %v", name, movedPlayers)
	}

	// --- B. locate server and set status to "restarting" ---
	mu.Lock()
	srv, exists := serverMap[name]
	if !exists {
		mu.Unlock()
		http.Error(w, fmt.Sprintf("Server '%s' not found", name), http.StatusNotFound)
		return
	}
	if srv == nil {
		mu.Unlock()
		http.Error(w, fmt.Sprintf("Server '%s' is already stopped. Use 'start' instead.", name), http.StatusBadRequest)
		return
	}

	// Mark as restarting
	srv.Status = "restarting"
	log.Printf("Server '%s' status set to 'restarting'", name)

	// copy needed fields and release lock
	port := srv.Port
	mu.Unlock()

	// compute server dir (same convention used when starting)
	dir := fmt.Sprintf("paper_server_%d", port)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		http.Error(w, fmt.Sprintf("Server directory does not exist: %s", dir), http.StatusBadRequest)
		return
	}

	// --- 1. Stop Server Gracefully ---
	if err := stopServerHold(name, srv); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// --- Server is now stopped and de-registered ---

	// --- 2. Server Restart ---
	if err := startHeldServer(name, port, dir); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// --- C. After successful restart: ask proxy to move players BACK to this server ---
	if len(movedPlayers) > 0 {
		// Small sleep to give the restarted server a moment to accept connections
		time.Sleep(1 * time.Second)

		proxyUrl, err := url.Parse(proxyApiHost + "/move_list_to")
		if err != nil {
			log.Printf("CRITICAL: Failed to parse proxyApiHost URL for /move_list: %v", err)
		} else {
			q := url.Values{}
			// join player list with commas (API expected format: players=name1,name2,...)
			q.Add("players", strings.Join(movedPlayers, ","))
			q.Add("server", name)
			proxyUrl.RawQuery = q.Encode()

			log.Printf("Requesting proxy to move players BACK to '%s' (proxy endpoint: %s)", name, proxyUrl.String())
			resp, err := client.Get(proxyUrl.String())
			if err != nil {
				// Do NOT fail the restart because of a proxy notification error; only log it.
				log.Printf("ERROR: Failed to call proxy /move_list for '%s': %v", name, err)
			} else {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					log.Printf("ERROR: Proxy returned non-OK status (%d) for /move_list: %s", resp.StatusCode, string(body))
				} else {
					log.Printf("Proxy /move_list response: %s", string(body))
				}
			}
		}
	} else {
		log.Printf("No players were moved away from '%s' earlier; skipping /move_list.", name)
	}

	// Final response: restart succeeded (proxy calls were attempted)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf("World '%s' restarted on port %d", name, port)))
}

func main() {

	err := godotenv.Load("../.env")
	if err != nil {
		log.Fatalf("Error loading .env file: %v", err)
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		log.Fatal("GITHUB_TOKEN not found in environment")
	}

	http.HandleFunc("/system", systemHandler)
	http.HandleFunc("/start-server", startServerHandler)
	http.HandleFunc("/stop-server", stopServerHandler)
	http.HandleFunc("/save-instance", saveWorldHandler)
	http.HandleFunc("/restart-instance", restartWorldHandler)

	port := 8000
	log.Printf("Server running on :3 http://localhost:%d\n", port)
	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
