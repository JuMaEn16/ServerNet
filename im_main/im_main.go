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
	"syscall"
	"time"
)

const (
	repoOwner = "JuMaEn16"
	repoName  = "ServerNet"
	// path inside the repo / zip to the subtree we care about:
	watchedSubdir = "im_main/instance_manager"
	// local directory name to place the subtree into:
	watchedSubdirLocal = "instance_manager"
	versionFileName    = ".current_version"
	httpTimeout        = 60 * time.Second
)

var (
	ErrRemoteVersionNotFound = errors.New("remote version file not found")
	ErrZipballNotFound       = errors.New("zipball not found (404) — repo may be private or removed")
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

	localVersion, _ := readLocalVersion()

	remoteVersion, err := fetchRemoteVersionContent(versionFileName)
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

func readLocalVersion() (string, error) {
	b, err := os.ReadFile(versionFileName)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func writeLocalVersion(content string) error {
	return os.WriteFile(versionFileName, []byte(strings.TrimSpace(content)), 0644)
}

func fetchRemoteVersionContent(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", repoOwner, repoName, path)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	// repo is public now — no Authorization header

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", ErrRemoteVersionNotFound
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("github api error: %s - %s", resp.Status, string(body))
	}

	var content ghContent
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&content); err != nil {
		return "", err
	}

	if content.Encoding == "base64" {
		decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(content.Content, "\n", ""))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(decoded)), nil
	}
	return strings.TrimSpace(content.Content), nil
}

func fetchLatestCommitSHA() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/commits?per_page=1", repoOwner, repoName)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("github api error fetching commits: %s - %s", resp.Status, string(body))
	}

	var arr []struct {
		SHA string `json:"sha"`
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&arr); err != nil {
		return "", err
	}
	if len(arr) == 0 || arr[0].SHA == "" {
		return "", errors.New("no commits returned")
	}
	return arr[0].SHA, nil
}

func updateInstanceManager() error {
	// Try zipball download first
	err := downloadAndExtractZipball()
	if err == nil {
		return nil
	}

	// If zipball not found, instruct user and attempt git-clone fallback.
	if errors.Is(err, ErrZipballNotFound) {
		return fmt.Errorf("%w\n\nThe repository zipball was not found. Ensure the repository %s/%s exists and is public", ErrZipballNotFound, repoOwner, repoName)
	}

	// Otherwise try git-clone fallback
	log.Println("Zipball download failed, attempting git clone fallback...")
	if err := cloneAndCopySubdir(); err != nil {
		return fmt.Errorf("git clone fallback failed: %w (original zipball error: %v)", err, err)
	}
	return nil
}

func downloadAndExtractZipball() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*httpTimeout)
	defer cancel()

	zipURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/zipball", repoOwner, repoName)
	req, _ := http.NewRequestWithContext(ctx, "GET", zipURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	// public repo => no Authorization header

	client := &http.Client{
		Timeout: 10 * httpTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// no special header copying required for public repos
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ErrZipballNotFound
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to download zipball: %s - %s", resp.Status, string(body))
	}

	tmpZipFile, err := os.CreateTemp("", "repo-zip-*.zip")
	if err != nil {
		return err
	}
	defer func() {
		tmpZipFile.Close()
		os.Remove(tmpZipFile.Name())
	}()

	if _, err := io.Copy(tmpZipFile, resp.Body); err != nil {
		return err
	}
	if _, err := tmpZipFile.Seek(0, io.SeekStart); err != nil {
		return err
	}

	stat, err := tmpZipFile.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(tmpZipFile, stat.Size())
	if err != nil {
		return err
	}

	tempDir, err := os.MkdirTemp("", "repo-extract-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	extractedAny := false
	for _, f := range zr.File {
		fpath := f.Name
		parts := strings.SplitN(fpath, "/", 2)
		if len(parts) < 2 {
			continue
		}
		rest := parts[1]
		if !strings.HasPrefix(rest, watchedSubdir+"/") && rest != watchedSubdir {
			continue
		}
		rel := strings.TrimPrefix(rest, watchedSubdir+"/")
		// place into watchedSubdirLocal inside our temp extraction dir
		destPath := filepath.Join(tempDir, watchedSubdirLocal, rel)

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(destPath, 0755); err != nil {
				return err
			}
			continue
		} else {
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				return err
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			outf, err := os.Create(destPath)
			if err != nil {
				rc.Close()
				return err
			}
			_, err = io.Copy(outf, rc)
			rc.Close()
			outf.Close()
			if err != nil {
				return err
			}
			_ = os.Chmod(destPath, f.Mode())
			extractedAny = true
		}
	}

	if !extractedAny {
		return fmt.Errorf("didn't find %s in repository archive", watchedSubdir)
	}

	// Replace local watchedSubdirLocal atomically: remove old and move new into place
	if _, err := os.Stat(watchedSubdirLocal); err == nil {
		backupDir, err := os.MkdirTemp("", "instance_manager-backup-*")
		if err != nil {
			return err
		}
		if err := moveDirAtomic(watchedSubdirLocal, filepath.Join(backupDir, watchedSubdirLocal)); err != nil {
			_ = os.RemoveAll(backupDir)
			return fmt.Errorf("failed to move old %s to backup: %w", watchedSubdirLocal, err)
		}
		defer func() {
			_ = os.RemoveAll(backupDir)
		}()
	}

	newPath := filepath.Join(tempDir, watchedSubdirLocal)
	if err := moveDirAtomic(newPath, watchedSubdirLocal); err != nil {
		return fmt.Errorf("failed to move new %s into place: %w", watchedSubdirLocal, err)
	}

	log.Println("Successfully updated", watchedSubdirLocal, "via zipball")
	return nil
}

func cloneAndCopySubdir() error {
	tmpDir, err := os.MkdirTemp("", "repo-clone-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	cloneURL := fmt.Sprintf("https://github.com/%s/%s.git", repoOwner, repoName)

	// Clone shallow to tmpDir.
	cmd := exec.Command("git", "clone", "--depth=1", "--single-branch", cloneURL, tmpDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone failed: %w", err)
	}

	src := filepath.Join(tmpDir, watchedSubdir)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("cloned repo does not contain %s: %w", watchedSubdir, err)
	}

	// Replace existing watchedSubdirLocal atomically
	if _, err := os.Stat(watchedSubdirLocal); err == nil {
		backupDir, err := os.MkdirTemp("", "instance_manager-backup-*")
		if err != nil {
			return err
		}
		if err := moveDirAtomic(watchedSubdirLocal, filepath.Join(backupDir, watchedSubdirLocal)); err != nil {
			_ = os.RemoveAll(backupDir)
			return fmt.Errorf("failed to move old %s to backup: %w", watchedSubdirLocal, err)
		}
		defer func() { _ = os.RemoveAll(backupDir) }()
	}

	if err := moveDirAtomic(src, watchedSubdirLocal); err != nil {
		return fmt.Errorf("failed to move cloned %s into place: %w", watchedSubdirLocal, err)
	}

	log.Println("Successfully updated", watchedSubdirLocal, "via git clone fallback")
	return nil
}

// moveDirAtomic tries to rename src->dest. If rename fails with EXDEV, it copies src->dest and removes src.
func moveDirAtomic(src, dest string) error {
	// try rename first
	if err := os.Rename(src, dest); err == nil {
		return nil
	} else {
		// if it's not a link error with EXDEV, return the error
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

	log.Printf("Running `go run .` in ./%s ...\n", watchedSubdirLocal)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go run failed: %w", err)
	}
	return nil
}
