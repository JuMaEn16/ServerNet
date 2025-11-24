package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
)

type Instance struct {
	Name        string   `json:"name"`
	Players     []string `json:"players"`
	PlayerCount int64    `json:"player_count"`
	TPS         int8     `json:"tps"`
	Port        int      `json:"port"`
	Status      string   `json:"status"`
}

type InstanceManager struct {
	State      string     `json:"state"`
	Domain     string     `json:"domain"`
	Name       string     `json:"name"`
	CPUPercent float64    `json:"cpu_percent,omitempty"`
	RAMUsedMB  uint64     `json:"ram_used_mb,omitempty"`
	RAMTotalMB uint64     `json:"ram_total_mb,omitempty"`
	Instances  []Instance `json:"instances,omitempty"`
}

type Proxy struct {
	State      string     `json:"state"`
	CPUPercent float64    `json:"cpu_percent,omitempty"`
	RAMUsedMB  uint64     `json:"ram_used_mb,omitempty"`
	RAMTotalMB uint64     `json:"ram_total_mb,omitempty"`
	Instances  []Instance `json:"instances,omitempty"`
}

type SystemInfo struct {
	CPUPercent float64    `json:"cpu_percent,omitempty"`
	RAMUsedMB  uint64     `json:"ram_used_mb,omitempty"`
	RAMTotalMB uint64     `json:"ram_total_mb,omitempty"`
	Instances  []Instance `json:"instances,omitempty"`
}

type ConfigIM struct {
	Domain string `json:"domain"`
	Name   string `json:"name"`
}

type ProxyServerInfo struct {
	Name    string  `json:"name"`
	Players float64 `json:"players"` // Use float64 for JSON number safety
	TPS     float64 `json:"tps"`
}
type ProxyStatus struct {
	PlayersTotal int               `json:"players_total"`
	ProxyLatency int               `json:"proxy_latency"`
	Servers      []ProxyServerInfo `json:"servers"`
	Error        string            `json:"error,omitempty"`
}

// GlobalSummary is the combined response for the new handler.
type GlobalSummary struct {
	Proxy       ProxyStatus       `json:"proxy"` // Changed from map[string]interface{}
	LocalSystem SystemInfo        `json:"system"`
	Managers    []InstanceManager `json:"managers"`
}

var (
	instanceManagers []InstanceManager
	configFile       = "ims_config.json"
	mu               sync.Mutex
	httpClient       = &http.Client{Timeout: 5 * time.Second}
)

// Load instance managers from config
func loadConfig() {
	file, err := os.ReadFile(configFile)
	if err != nil {
		if os.IsNotExist(err) {
			instanceManagers = []InstanceManager{}
			return
		}
		log.Fatalf("Failed to read config: %v", err)
	}

	var cfg []ConfigIM
	if err := json.Unmarshal(file, &cfg); err != nil {
		log.Fatalf("Failed to parse config: %v", err)
	}

	for _, c := range cfg {
		instanceManagers = append(instanceManagers, InstanceManager{
			Domain: c.Domain,
			Name:   c.Name,
		})
	}
}

// Save instance managers to config (only Domain + Name)
func saveConfig() {
	mu.Lock()
	defer mu.Unlock()
	var cfg []ConfigIM
	for _, im := range instanceManagers {
		cfg = append(cfg, ConfigIM{
			Domain: im.Domain,
			Name:   im.Name,
		})
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		log.Printf("Failed to marshal config: %v", err)
		return
	}

	if err := os.WriteFile(configFile, data, 0644); err != nil {
		log.Printf("Failed to write config file: %v", err)
	}
}

// Endpoint to get instance summary
func fetchLocalProxyStatus() ProxyStatus {
	var proxyResp ProxyStatus
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://localhost:8081/status")
	if err != nil {
		proxyResp.Error = err.Error()
	} else {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(&proxyResp); err != nil {
			proxyResp.Error = "invalid JSON from proxy: " + err.Error()
		}
	}
	return proxyResp
}

func fetchLocalSystemInfo() SystemInfo {
	cpuPct, err := getCPUPercent()
	if err != nil {
		cpuPct = 0.0
	}
	usedMB, totalMB, err := getRAMInfo()
	if err != nil {
		usedMB = 0
		totalMB = 0
	}
	return SystemInfo{
		CPUPercent: cpuPct,
		RAMUsedMB:  usedMB,
		RAMTotalMB: totalMB,
	}
}

func fetchInstanceSummaries() []InstanceManager {
	mu.Lock()
	ims := make([]InstanceManager, len(instanceManagers))
	copy(ims, instanceManagers) // work on a copy
	mu.Unlock()

	var result []InstanceManager
	var wg sync.WaitGroup
	// Channel to collect results concurrently
	ch := make(chan InstanceManager, len(ims))

	for _, im := range ims {
		wg.Add(1)
		go func(im InstanceManager) {
			defer wg.Done()
			url := fmt.Sprintf("http://%s/system", im.Domain)
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Get(url)
			if err != nil {
				log.Printf("%s is Offline", im.Domain)
				im.State = "Offline"
				ch <- im
				return
			}
			defer resp.Body.Close()

			data, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Printf("Failed to read /system from %s: %v", im.Domain, err)
				im.State = "Warning"
				ch <- im
				return
			}

			var sys SystemInfo
			if err := json.Unmarshal(data, &sys); err != nil {
				log.Printf("Failed to decode /system JSON from %s: %v", im.Domain, err)
				im.State = "Warning"
				ch <- im
				return
			}

			im.CPUPercent = sys.CPUPercent
			im.RAMUsedMB = sys.RAMUsedMB
			im.RAMTotalMB = sys.RAMTotalMB
			im.Instances = sys.Instances
			im.State = "Online"
			ch <- im
		}(im)
	}

	// Wait for all goroutines to finish
	wg.Wait()
	close(ch)

	// Collect results from channel
	for im := range ch {
		result = append(result, im)
	}

	return result
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var summary GlobalSummary
	var wg sync.WaitGroup

	wg.Add(3)

	// Goroutine 1: Fetch local proxy status
	go func() {
		defer wg.Done()
		summary.Proxy = fetchLocalProxyStatus()
	}()

	// Goroutine 2: Fetch local system info
	go func() {
		defer wg.Done()
		summary.LocalSystem = fetchLocalSystemInfo()
	}()

	// Goroutine 3: Fetch remote instance summaries
	go func() {
		defer wg.Done()
		summary.Managers = fetchInstanceSummaries()
	}()

	// Wait for all tasks to complete
	wg.Wait()

	// --- NEW: Merge Logic ---
	// Create a map of proxy server info for easy lookup by name
	proxyServerInfo := make(map[string]ProxyServerInfo)
	if summary.Proxy.Servers != nil {
		for _, proxyServer := range summary.Proxy.Servers {
			proxyServerInfo[proxyServer.Name] = proxyServer
		}
	}

	// Iterate through managers and instances to update them
	// We must use indices here to modify the structs within the slice
	for mIdx := range summary.Managers {
		for iIdx := range summary.Managers[mIdx].Instances {
			// Get a pointer to the instance to modify it
			instance := &summary.Managers[mIdx].Instances[iIdx]

			// Check if we have info for this instance from the proxy
			if info, ok := proxyServerInfo[instance.Name]; ok {
				// Found a match! Update PlayerCount and TPS.
				instance.PlayerCount = int64(info.Players)
				instance.TPS = int8(info.TPS)
			}
		}
	}
	// --- End of Merge Logic ---

	if err := json.NewEncoder(w).Encode(summary); err != nil {
		log.Printf("Failed to encode global summary: %v", err)
		http.Error(w, "failed to encode response: "+err.Error(), http.StatusInternalServerError)
	}
}

// Endpoint to create a new instance manager
func createIM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var im InstanceManager
	if err := json.NewDecoder(r.Body).Decode(&im); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	mu.Lock()
	instanceManagers = append(instanceManagers, InstanceManager{
		Domain: im.Domain,
		Name:   im.Name,
	})
	mu.Unlock()

	saveConfig()

	log.Printf("New IM '%s' added", im.Name)

	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "Instance manager '%s' created", im.Name)
}

func deleteIM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Domain string `json:"domain"`
		Name   string `json:"name"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	mu.Lock()

	index := -1
	for i, im := range instanceManagers {
		if im.Domain == req.Domain && im.Name == req.Name {
			index = i
			break
		}
	}

	if index == -1 {
		mu.Unlock()
		http.Error(w, "Instance Manager not found", http.StatusNotFound)
		return
	}

	// Remove element
	instanceManagers = append(instanceManagers[:index], instanceManagers[index+1:]...)
	mu.Unlock()

	saveConfig()

	log.Printf("IM '%s' deleted", req.Name)

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Instance manager '%s' deleted", req.Name)
}

// getCPUPercent uses gopsutil to sample CPU usage percentage.
func getCPUPercent() (float64, error) {
	// cpu.Percent takes an interval and whether to get per-cpu. Setting interval > 0 blocks for the interval.
	percents, err := cpu.Percent(500*time.Millisecond, false)
	if err != nil {
		return 0, err
	}
	if len(percents) == 0 {
		return 0, nil
	}
	return percents[0], nil
}

// getRAMInfo uses gopsutil to get used / total memory in MB.
func getRAMInfo() (usedMB uint64, totalMB uint64, err error) {
	vm, err := mem.VirtualMemory()
	if err != nil {
		return 0, 0, err
	}
	totalMB = vm.Total / 1024 / 1024
	usedMB = (vm.Total - vm.Available) / 1024 / 1024
	return usedMB, totalMB, nil
}

// ensureLobby checks proxy /status for a "lobby" server, and if missing performs:
// - call local /instance_summary
// - pick least-loaded IM
// - call IM /start-server?name=lobby and parse returned port
// - call proxy /add_server with name=lobby host=<im.Domain> port=<port>
func hasStringInSlice(s []interface{}, name string) bool {
	for _, it := range s {
		if m, ok := it.(map[string]interface{}); ok {
			if nameVal, ok := m["name"]; ok {
				if nameStr, ok := nameVal.(string); ok && nameStr == name {
					return true
				}
			}
		}
	}
	return false
}

// Try multiple proxy endpoints/formats and return true if lobby exists
func proxyHasInstance(name string) (bool, error) {
	endpoints := []string{
		"http://localhost:8081/status",
		"http://localhost:8081/list_servers",
	}

	for _, ep := range endpoints {
		resp, err := httpClient.Get(ep)
		if err != nil {
			log.Printf("proxy %s error: %v", ep, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		// 1) Try { "proxy": { "servers": [...] } } or { "servers": [...] }
		var top map[string]interface{}
		if err := json.Unmarshal(body, &top); err == nil {
			// proxy.servers
			if proxyRaw, ok := top["proxy"].(map[string]interface{}); ok {
				if serversRaw, ok := proxyRaw["servers"].([]interface{}); ok {
					if hasStringInSlice(serversRaw, name) {
						return true, nil
					}
				}
			}
			// top-level servers
			if serversRaw, ok := top["servers"].([]interface{}); ok {
				if hasStringInSlice(serversRaw, name) {
					return true, nil
				}
			}
		}

		// 2) Try plain array format: [ { "name": "...", ...}, ... ]
		var arr []interface{}
		if err := json.Unmarshal(body, &arr); err == nil {
			if hasStringInSlice(arr, name) {
				return true, nil
			}
		}

		// for debugging: log body when no lobby was found for this endpoint
		log.Printf("proxy %s returned but no lobby found; body: %s", ep, string(body))
	}

	// no lobby found in any endpoint
	return false, nil
}

func getInstanceSummary() ([]InstanceManager, error) {
	// Updated to call the new /status endpoint
	resp, err := httpClient.Get("http://localhost:8080/status")
	if err != nil {
		return nil, fmt.Errorf("failed to call /status: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/status returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read /status body: %v", err)
	}

	// Unmarshal into the new combined GlobalSummary struct
	var summary GlobalSummary
	if err := json.Unmarshal(body, &summary); err != nil {
		return nil, fmt.Errorf("failed to decode /status summary: %v", err)
	}

	// Return just the Managers slice, as the original function did
	return summary.Managers, nil
}

// registerInstanceToProxy tells the proxy to add the server.
func registerInstanceToProxy(name, domain string, port int) {
	host, _, err := net.SplitHostPort(domain)
	if err != nil {
		host = domain // fallback if no port in domain (e.g., "im1.example.com")
	}

	addURL := fmt.Sprintf(
		"http://localhost:8081/add_server?name=%s&host=%s&port=%d",
		url.QueryEscape(name),
		url.QueryEscape(host),
		port,
	)

	r, err := httpClient.Get(addURL)
	if err != nil {
		log.Printf("Failed to add existing instance '%s' to proxy: %v", name, err)
		return
	}
	defer r.Body.Close()

	rb, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK {
		log.Printf("Proxy /add_server error %d: %s", r.StatusCode, string(rb))
		return
	}

	log.Printf("Instance '%s' registered to proxy (host: %s, port: %d).", name, host, port)
}

func stopServerOnIM(domain, name string) error {
	stopURL := fmt.Sprintf("http://%s/stop-server?name=%s", domain, url.QueryEscape(name))
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(stopURL)
	if err != nil {
		return fmt.Errorf("request to IM %s failed: %w", stopURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("IM stop-server returned status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// removeServerFromProxy requests the proxy to remove the server from its registration.
// NOTE: adjust proxyAdminAddr to the actual proxy admin API host:port.
func removeServerFromProxy(name string) error {
	proxyAdminAddr := "http://localhost:8081"
	removeURL := fmt.Sprintf("%s/remove_server?name=%s", proxyAdminAddr, url.QueryEscape(name))

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(removeURL)
	if err != nil {
		return fmt.Errorf("request to proxy %s failed: %w", removeURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy remove_server returned status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// cleanupEmptyServers scans all IMs and stops/unregisters servers with PlayerCount == 0.
// It explicitly skips any server named "lobby".
func cleanupEmptyServers() {
	ims, err := getInstanceSummary()
	fmt.Println(ims)
	if err != nil {
		log.Printf("cleanupEmptyServers: failed to fetch instance summary: %v", err)
		return
	}
	if len(ims) == 0 {
		// nothing to do
		return
	}

	for _, im := range ims {
		for _, inst := range im.Instances {
			// skip lobby by name
			if inst.Name == "lobby" {
				continue
			}
			// only consider servers that are running/started (you can extend statuses if desired)
			if inst.PlayerCount == 0 && (inst.Status == "running" || inst.Status == "started") {
				log.Printf("cleanup: found empty instance '%s' on %s (port %d). Attempting to stop and unregister.", inst.Name, im.Domain, inst.Port)

				// 1) Stop the server on the IM
				if err := stopServerOnIM(im.Domain, inst.Name); err != nil {
					log.Printf("cleanup: failed to stop instance '%s' on %s: %v", inst.Name, im.Domain, err)
					// continue to next instance — don't attempt remove from proxy if stop failed
					continue
				}
				log.Printf("cleanup: stop-server request sent for '%s' on %s", inst.Name, im.Domain)

				// Optional: wait/poll until instance status changes / disappears in instance summary.
				// Simple delay gives the IM time to tear down the server before removing from proxy.
				time.Sleep(2 * time.Second)

				// 2) Remove from proxy
				if err := removeServerFromProxy(inst.Name); err != nil {
					log.Printf("cleanup: failed to remove '%s' from proxy: %v", inst.Name, err)
					// note: server is stopped on IM, but proxy removal failed — you may want retry logic here
					continue
				}
				log.Printf("cleanup: successfully stopped and unregistered instance '%s'", inst.Name)
			}
		}
	}
}

// waitForInstance polls the instance summary until an instance is "running".
func waitForInstance(name string) {
	log.Printf("Waiting for instance '%s' to finish 'restarting'...", name)

	// Poll for 60 seconds (12 retries * 5 seconds)
	for i := 0; i < 12; i++ {
		time.Sleep(5 * time.Second)

		ims, err := getInstanceSummary()
		if err != nil {
			log.Printf("Error polling for instance '%s' status: %v", name, err)
			continue // Try again
		}

		found := false
		for _, im := range ims {
			for _, inst := range im.Instances {
				if inst.Name == name {
					found = true
					switch inst.Status {
					case "running":
						log.Printf("Instance '%s' is now 'running'. Registering.", name)
						registerInstanceToProxy(name, im.Domain, inst.Port)
						return // Success
					case "restarting":
						log.Printf("... instance '%s' is still 'restarting'.", name)
					default:
						log.Printf("Instance '%s' changed to unexpected status '%s' while waiting. Aborting.", name, inst.Status)
						return // Error
					}
					break // Found instance, stop inner loop
				}
			}
			if found {
				break // Found instance, stop outer loop
			}
		}

		if !found {
			log.Printf("Instance '%s' disappeared during restart poll. Aborting.", name)
			return // Error
		}
	}

	log.Printf("Timed out waiting for instance '%s' to restart.", name)
}

func ensureInstance(name string) {
	// 1) Check if instance is already registered in proxy
	found, err := proxyHasInstance(name)
	if err != nil {
		log.Printf("proxy check error: %v", err)
		return
	}
	if found {
		return
	}

	// 2) Fetch instance summary
	ims, err := getInstanceSummary()
	if err != nil {
		log.Printf("Failed to fetch instance summary: %v", err)
		return
	}
	if len(ims) == 0 {
		log.Printf("No instance managers available from /instance_summary.")
		return
	}

	// 3) Check if the instance is already running anywhere
	for _, im := range ims {
		for _, inst := range im.Instances {
			if inst.Name == name {
				// --- THIS IS THE MODIFIED LOGIC ---
				switch inst.Status {
				case "running":
					log.Printf("Found existing 'running' instance '%s' on %s. Registering with proxy.", name, im.Domain)
					registerInstanceToProxy(name, im.Domain, inst.Port)
					return // Success
				case "started":
					log.Printf("Found existing 'running' instance '%s' on %s. Registering with proxy.", name, im.Domain)
					registerInstanceToProxy(name, im.Domain, inst.Port)
					return // Success
				case "restarting":
					log.Printf("Found 'restarting' instance '%s' on %s.", name, im.Domain)
					waitForInstance(name) // This function will wait, then register or time out
					return
				default:
					// Any other status: "saving", "stopped", "creating", etc.
					log.Printf("Error: Instance '%s' found on %s but has an unhandled status: '%s'. Won't start a new one.", name, im.Domain, inst.Status)
					return // Return with error
				}
				// --- END OF MODIFIED LOGIC ---
			}
		}
	}

	// 4) No existing instance found: pick least-loaded IM
	filtered := make([]InstanceManager, 0, len(ims))
	for _, im := range ims {
		if im.CPUPercent != 0 {
			filtered = append(filtered, im)
		}
	}

	if len(filtered) == 0 {
		log.Printf("No ONLINE instance managers available to start server.")
		return
	}

	// Sort by CPU (asc), then free RAM (desc)
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].CPUPercent == filtered[j].CPUPercent {
			freeRAMi := filtered[i].RAMTotalMB - filtered[i].RAMUsedMB
			freeRAMj := filtered[j].RAMTotalMB - filtered[j].RAMUsedMB
			return freeRAMi > freeRAMj
		}
		return filtered[i].CPUPercent < filtered[j].CPUPercent
	})

	selected := filtered[0]
	log.Printf("Selected IM %s (%s) with CPU %.2f%% RAM used %dMB",
		selected.Name, selected.Domain, selected.CPUPercent, selected.RAMUsedMB)

	// 5) Start the instance via /start-server
	startURL := fmt.Sprintf("http://%s/start-server?name=%s", selected.Domain, url.QueryEscape(name))
	// Longer timeout for starting a server
	client := &http.Client{Timeout: 90 * time.Second} // Increased timeout
	resp3, err := client.Get(startURL)
	if err != nil {
		log.Printf("Failed to call %s: %v", startURL, err)
		return
	}
	body3, err := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if err != nil {
		log.Printf("Failed to read start-server response: %v", err)
		return
	}

	if resp3.StatusCode != http.StatusOK {
		log.Printf("start-server failed with status %d: %s", resp3.StatusCode, string(body3))
		return
	}

	// 6) Parse port from response
	port, parseErr := parsePortFromResponse(body3)
	if parseErr != nil {
		log.Printf("Failed to parse port from start-server response: %v -- body: %s", parseErr, string(body3))
		return
	}
	log.Printf("Started instance '%s' on %s:%d", name, selected.Domain, port)

	// 7) Register the new instance with the proxy
	registerInstanceToProxy(name, selected.Domain, port)

	log.Printf("Proxy /add_server success for new instance '%s'.", name)
}

// parsePortFromResponse tries to decode JSON {"port":N} or extract first integer in the body as port.
func parsePortFromResponse(body []byte) (int, error) {
	// try JSON
	var j map[string]interface{}
	if err := json.Unmarshal(body, &j); err == nil {
		if p, ok := j["port"]; ok {
			switch v := p.(type) {
			case float64:
				return int(v), nil
			case int:
				return v, nil
			case string:
				if pi, err := strconv.Atoi(v); err == nil {
					return pi, nil
				}
			}
		}
	}

	// try to find digits in the plain text response
	re := regexp.MustCompile(`\b([0-9]{2,6})\b`)
	m := re.FindSubmatch(body)
	if len(m) > 1 {
		if p, err := strconv.Atoi(string(m[1])); err == nil {
			return p, nil
		}
	}

	return 0, fmt.Errorf("no port found in response")
}

func moveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name   string `json:"name"`
		Server string `json:"server"`
	}

	ct := r.Header.Get("Content-Type")
	// Decide how to parse body based on content type (handle charset too).
	switch {
	case strings.Contains(ct, "application/json"):
		// JSON body
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
			return
		}
	default:
		// Fallback to parsing form data (application/x-www-form-urlencoded)
		// ParseForm handles both "POST" form bodies and URL query parameters.
		if err := r.ParseForm(); err != nil {
			http.Error(w, fmt.Sprintf("Failed to parse form: %v", err), http.StatusBadRequest)
			return
		}
		req.Name = r.FormValue("name")
		req.Server = r.FormValue("server")
	}

	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Server) == "" {
		http.Error(w, "Both 'name' and 'server' are required", http.StatusBadRequest)
		return
	}

	// Ensure the destination instance exists (your function; assumed defined elsewhere).
	ensureInstance(req.Server)

	// Forward to local move_to endpoint.
	endpoint := "http://localhost:8081/move_to"
	params := url.Values{}
	params.Set("player", req.Name)
	params.Set("server", req.Server)

	resp, err := http.Get(endpoint + "?" + params.Encode())
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to call /move_to: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("/move_to returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body))), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Moved player %s to server %s", req.Name, req.Server)
}

func moveAllHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Origin      string `json:"origin"`
		Destination string `json:"destination"`
	}

	ct := r.Header.Get("Content-Type")
	// Decide how to parse body based on content type (handle charset too).
	switch {
	case strings.Contains(ct, "application/json"):
		// JSON body
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
			return
		}
	default:
		// Fallback to parsing form data (application/x-www-form-urlencoded)
		// ParseForm handles both "POST" form bodies and URL query parameters.
		if err := r.ParseForm(); err != nil {
			http.Error(w, fmt.Sprintf("Failed to parse form: %v", err), http.StatusBadRequest)
			return
		}
		req.Origin = r.FormValue("origin")
		req.Destination = r.FormValue("destination")
	}

	if strings.TrimSpace(req.Origin) == "" || strings.TrimSpace(req.Destination) == "" {
		http.Error(w, "Both 'origin' and 'destination' are required", http.StatusBadRequest)
		return
	}

	// Ensure the destination instance exists (your function; assumed defined elsewhere).
	ensureInstance(req.Origin)

	// Forward to local move_to endpoint.
	endpoint := "http://localhost:8081/move_from_to"
	params := url.Values{}
	params.Set("origin", req.Origin)
	params.Set("destination", req.Destination)

	resp, err := http.Get(endpoint + "?" + params.Encode())
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to call /move_from_to: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("/move_to returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body))), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Moved from %s to server %s", req.Origin, req.Destination)
}

func runCommand(dir string, command string, args ...string) {
	go func() {
		cmd := exec.Command(command, args...)
		cmd.Dir = dir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			log.Printf("Failed to run %s in %s: %v", command, dir, err)
		}
	}()
}

func InstanceActionHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Println("Got request")

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	type ActionRequest struct {
		Domain string `json:"domain"`
		Name   string `json:"name"`
		Action string `json:"action"`
	}

	var req ActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if req.Domain == "" || req.Name == "" {
		http.Error(w, "domain and name are required", http.StatusBadRequest)
		return
	}
	fmt.Println(req)

	var endpoint string
	switch req.Action {
	case "restart":
		endpoint = "/restart-instance"
	case "save":
		endpoint = "/save-instance"
	default:
		http.Error(w, "invalid action", http.StatusBadRequest)
		return
	}

	// Correct format: http://domain/restart-instance?name=XYZ
	targetURL := url.URL{
		Scheme: "http",
		Host:   req.Domain,
		Path:   endpoint,
	}
	query := targetURL.Query()
	query.Set("name", req.Name)
	targetURL.RawQuery = query.Encode()

	fmt.Println("Sending request to:", targetURL.String())

	client := &http.Client{Timeout: 5 * time.Second}

	// send empty POST request
	resp, err := client.Post(targetURL.String(), "application/json", nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to contact instance: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("instance returned: %s", resp.Status), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message": "action forwarded successfully",
	})
}

func runCommandWait(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run() // waits until the command finishes
}

func main() {
	loadConfig()

	go func() {
		// Step 1: npm install (blocking inside goroutine)
		if err := runCommandWait("./website", "npm", "install"); err != nil {
			log.Printf("npm install failed: %v", err)
			return
		}

		// Step 2: npm run dev (also blocking inside goroutine)
		// This process usually does not exit until you stop the program.
		if err := runCommandWait("./website", "npm", "run", "dev"); err != nil {
			log.Printf("npm run dev failed: %v", err)
			return
		}
	}()

	runCommand("./proxy", "java", "-jar", "velocity.jar")

	go func() {
		time.Sleep(10 * time.Second) // let server come up
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			ensureInstance("lobby")
			<-ticker.C
		}
	}()

	go func() {
		// wait a bit for system to become healthy
		time.Sleep(7 * time.Second)
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			cleanupEmptyServers()
			<-ticker.C
		}
	}()

	http.HandleFunc("/status", statusHandler)
	http.HandleFunc("/create_im", createIM)
	http.HandleFunc("/delete_im", deleteIM)
	http.HandleFunc("/move", moveHandler)
	http.HandleFunc("/move_all", moveAllHandler)
	http.HandleFunc("/action", InstanceActionHandler)
	//http.HandleFunc("/restart-instance", restartWorldHandler)

	port := 8080
	log.Printf("Server running on http://localhost:%d/\n", port)
	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
