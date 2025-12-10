package main

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	repoOwner = "JuMaEn16"
	repoName  = "ServerNet"

	// path inside the repo / zip to the subtree we care about:
	watchedSubdir = "server_main/server_manager"
	// local directory name to place the subtree into:
	watchedSubdirLocal = "server_manager"

	versionFileName = ".current_version"
	httpTimeout     = 60 * time.Second
)

var (
	ErrRemoteVersionNotFound = errors.New("remote version file not found")
	ErrZipballNotFound       = errors.New("zipball not found (404) — repo may be private or removed")

	httpClient = &http.Client{Timeout: httpTimeout}

	githubToken = os.Getenv("GITHUB_TOKEN")

	// Child process tracking for restart API
	childMu  sync.Mutex
	childCmd *exec.Cmd
)

type ghContent struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Size     int    `json:"size"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Content  string `json:"content"`
	Sha      string `json:"sha"`
}

func main() {
	log.SetFlags(0)

	// Start tiny control HTTP server for restarts
	go startControlAPI()

	localVersion, _ := readLocalVersion()
	println(localVersion)

	remoteVersion, err := fetchRemoteVersionContent("server_main/" + versionFileName)
	if err != nil {
		if errors.Is(err, ErrRemoteVersionNotFound) {
			log.Println("Remote version file not found. Downloading newest instance_manager and creating local version file.")
			if err := updateInstanceManager(); err != nil {
				log.Fatalf("Update failed: %v", err)
			}

			sha, err := fetchLatestCommitSHA()
			if err != nil {
				log.Printf("Warning: could not fetch latest commit SHA: %v. Falling back to timestamp.", err)
				sha = time.Now().UTC().Format(time.RFC3339)
			}

			if err := writeLocalVersion(sha); err != nil {
				log.Printf("Warning: failed to write local version file: %v", err)
			}

			if err := runInstanceManager(); err != nil {
				log.Fatalf("Failed to run updated instance_manager: %v", err)
			}
			return
		}

		log.Printf("Warning: could not fetch remote %s: %v", versionFileName, err)
		if err := runInstanceManager(); err != nil {
			log.Fatalf("Failed to run local instance_manager: %v", err)
		}
		return
	}

	if strings.TrimSpace(localVersion) == strings.TrimSpace(remoteVersion) && localVersion != "" {
		log.Printf("Local: %s Remote: %s", strings.TrimSpace(localVersion), strings.TrimSpace(remoteVersion))
		log.Println("No update detected. Running local instance_manager...")
		if err := runInstanceManager(); err != nil {
			log.Fatalf("Failed to run local instance_manager: %v", err)
		}
		return
	}

	log.Println("Update detected (or local version missing). Downloading new instance_manager...")

	if err := updateInstanceManager(); err != nil {
		log.Fatalf("Update failed: %v", err)
	}

	if err := writeLocalVersion(remoteVersion); err != nil {
		log.Printf("Warning: failed to write local version file: %v", err)
	}

	if err := runInstanceManager(); err != nil {
		log.Fatalf("Failed to run updated instance_manager: %v", err)
	}
}

// ----------------- Control HTTP API & restart logic -----------------

func startControlAPI() {
	mux := http.NewServeMux()

	mux.HandleFunc("/restart-all", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("Restarting updater and instance_manager...\n"))

		// Do restart asynchronously so the response can flush
		go func() {
			// 1) stop current child
			stopChild()

			// 2) small delay just to let logs flush, etc.
			time.Sleep(1 * time.Second)

			// 3) exec ourselves
			if err := selfRestart(); err != nil {
				log.Printf("selfRestart failed: %v", err)
			}
		}()
	})

	srv := &http.Server{
		Addr:    "127.0.0.1:8090", // only listen on localhost
		Handler: mux,
	}

	log.Println("Control API listening on http://127.0.0.1:8090")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("control HTTP server error: %v", err)
	}
}

// stopChild tries to gracefully stop the child process, then force-kills if needed.
func stopChild() {
	childMu.Lock()
	cmd := childCmd
	childMu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}

	log.Println("Stopping child process...")

	// try graceful stop first
	_ = cmd.Process.Signal(os.Interrupt)

	// wait a bit and then force kill if still running
	time.Sleep(10 * time.Second)

	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		log.Println("Child did not stop in time; killing")
		_ = cmd.Process.Kill()
	}
}

// selfRestart replaces the current process with a new instance of the same binary.
func selfRestart() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Replace current process image; does not return on success.
	return syscall.Exec(exe, os.Args, os.Environ())
}

// ----------------- GitHub helpers -----------------

func addGitHubHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "ServerNet-updater")
	if githubToken != "" {
		req.Header.Set("Authorization", "Bearer "+githubToken)
	}
}

func fetchRemoteVersionContent(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", repoOwner, repoName, path)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	addGitHubHeaders(req)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", ErrRemoteVersionNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("unexpected status from GitHub contents API: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var gc ghContent
	if err := json.NewDecoder(resp.Body).Decode(&gc); err != nil {
		return "", err
	}

	if strings.ToLower(gc.Encoding) == "base64" {
		data, err := base64.StdEncoding.DecodeString(gc.Content)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}

	return gc.Content, nil
}

func fetchLatestCommitSHA() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/commits?per_page=1", repoOwner, repoName)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	addGitHubHeaders(req)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("unexpected status from GitHub commits API: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var commits []struct {
		Sha string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&commits); err != nil {
		return "", err
	}
	if len(commits) == 0 || commits[0].Sha == "" {
		return "", errors.New("no commits returned from GitHub API")
	}
	return commits[0].Sha, nil
}

// ----------------- Updating / zip handling -----------------

func updateInstanceManager() error {
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()

	zipPath, err := downloadZipball(ctx)
	if err != nil {
		return err
	}
	defer os.Remove(zipPath)

	tmpDir, err := os.MkdirTemp("", "servernet-update-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := extractWatchedSubdirFromZip(zipPath, tmpDir); err != nil {
		return fmt.Errorf("failed to extract watched subdir from zip: %w", err)
	}

	src := filepath.Join(tmpDir, watchedSubdirLocal)
	if err := moveOrCopyDir(src, watchedSubdirLocal); err != nil {
		return fmt.Errorf("failed to move/copy extracted dir into place: %w", err)
	}

	return nil
}

func downloadZipball(ctx context.Context) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/zipball", repoOwner, repoName)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	addGitHubHeaders(req)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", ErrZipballNotFound
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("unexpected status from GitHub zipball: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	tmpFile, err := os.CreateTemp("", "servernet-*.zip")
	if err != nil {
		return "", fmt.Errorf("failed to create temp zip file: %w", err)
	}
	defer tmpFile.Close()

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		return "", fmt.Errorf("failed to download zipball: %w", err)
	}

	return tmpFile.Name(), nil
}

// extractWatchedSubdirFromZip extracts the subtree watchedSubdir into destRoot/watchedSubdirLocal.
func extractWatchedSubdirFromZip(zipPath, destRoot string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("failed to open zip: %w", err)
	}
	defer r.Close()

	if len(r.File) == 0 {
		return errors.New("zipball appears to be empty")
	}

	// Figure out the top-level directory name inside the zip (e.g. "owner-repo-<sha>/...").
	firstName := r.File[0].Name
	top := strings.SplitN(firstName, "/", 2)[0]
	if top == "" {
		return fmt.Errorf("unexpected zip entry path: %q", firstName)
	}

	watchedPrefix := top + "/" + watchedSubdir + "/"
	destBase := filepath.Join(destRoot, watchedSubdirLocal)

	for _, f := range r.File {
		name := f.Name
		if !strings.HasPrefix(name, watchedPrefix) {
			continue
		}
		rel := strings.TrimPrefix(name, watchedPrefix)
		if rel == "" {
			// This is the directory entry for watchedSubdir itself
			continue
		}
		target := filepath.Join(destBase, rel)

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, f.Mode()); err != nil {
				return err
			}
			continue
		}

		// ensure parent dir exists
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}

		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode())
		if err != nil {
			_ = rc.Close()
			return err
		}

		if _, err := io.Copy(out, rc); err != nil {
			_ = rc.Close()
			_ = out.Close()
			return err
		}
		_ = rc.Close()
		_ = out.Close()
	}

	return nil
}

// moveOrCopyDir tries to os.Rename src -> dest, and if that fails with EXDEV, falls back to copy+remove.
func moveOrCopyDir(src, dest string) error {
	// remove existing dest
	if err := os.RemoveAll(dest); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing dest dir %s: %w", dest, err)
	}

	if err := os.Rename(src, dest); err != nil {
		// if it's a cross-device link error with EXDEV, return the error
		var linkErr *os.LinkError
		if errors.As(err, &linkErr) {
			if pe, ok := linkErr.Err.(syscall.Errno); ok && pe == syscall.EXDEV {
				// cross-device link error -> do copy
				if err := copyDir(src, dest); err != nil {
					return fmt.Errorf("copy during EXDEV fallback failed: %w", err)
				}
				// remove original
				if err := os.RemoveAll(src); err != nil {
					return fmt.Errorf("failed to remove original after copy: %w", err)
				}
				return nil
			}
		}
		return err
	}
	return nil
}

// copyDir recursively copies src to dest, preserving modes and symlinks.
func copyDir(src, dest string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)

		info, err := d.Info()
		if err != nil {
			return err
		}

		// directory
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}

		// handle symlinks
		if info.Mode()&os.ModeSymlink != 0 {
			linkDest, err := os.Readlink(path)
			if err != nil {
				return err
			}
			// ensure parent dir exists
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			// create symlink
			return os.Symlink(linkDest, target)
		}

		// regular file
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()

		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		defer out.Close()

		if _, err := io.Copy(out, in); err != nil {
			return err
		}
		// set file mode explicitly (in case umask etc)
		if err := os.Chmod(target, info.Mode()); err != nil {
			return err
		}
		return nil
	})
}

// ----------------- Version file + running child -----------------

func readLocalVersion() (string, error) {
	data, err := os.ReadFile(versionFileName)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func writeLocalVersion(version string) error {
	return os.WriteFile(versionFileName, []byte(version), 0644)
}

func runInstanceManager() error {
	if _, err := os.Stat(watchedSubdirLocal); err != nil {
		return fmt.Errorf("%s does not exist: %w", watchedSubdirLocal, err)
	}

	cmd := exec.Command("go", "run", ".")
	cmd.Dir = watchedSubdirLocal
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = os.Environ()

	// remember the child
	childMu.Lock()
	childCmd = cmd
	childMu.Unlock()

	log.Printf("Running `go run .` in ./%s ...\n", watchedSubdirLocal)
	err := cmd.Run()

	// child exited; clear it
	childMu.Lock()
	if childCmd == cmd {
		childCmd = nil
	}
	childMu.Unlock()

	if err != nil {
		return fmt.Errorf("go run failed: %w", err)
	}
	return nil
}
