package internal

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// SkillsLock tracks skill versions via content hashes.
type SkillsLock struct {
	Version int                   `json:"version"`
	Skills  map[string]SkillEntry `json:"skills"`
}

type SkillEntry struct {
	Hash  string            `json:"hash"`
	Files map[string]string `json:"files,omitempty"` // relative path → hash
}

// SkillManager manages skill files stored under looper/skills/.
type SkillManager struct {
	skillsDir  string // looper/skills/
	lockPath   string // looper/skills-lock.json
	mu         sync.RWMutex
	globalDirs []string
}

// NewSkillManager creates a manager rooted at the given skills directory.
func NewSkillManager(skillsDir string, globalDirs []string) *SkillManager {
	dirs := normalizeDirs(globalDirs)

	return &SkillManager{
		skillsDir:  skillsDir,
		lockPath:   filepath.Join(filepath.Dir(skillsDir), "skills-lock.json"),
		globalDirs: dirs,
	}
}

func normalizeDirs(globalDirs []string) []string {
	uniq := make(map[string]bool)
	var dirs []string
	for _, d := range globalDirs {
		d = strings.TrimSpace(d)
		if d == "" || uniq[d] {
			continue
		}
		uniq[d] = true
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}

func (m *SkillManager) SetGlobalDirs(globalDirs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.globalDirs = normalizeDirs(globalDirs)
}

func (m *SkillManager) GlobalDirs() []string {
	return m.snapshotGlobalDirs()
}

func (m *SkillManager) snapshotGlobalDirs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	dirs := make([]string, len(m.globalDirs))
	copy(dirs, m.globalDirs)
	return dirs
}

// EnsureSymlinks creates or updates symlinks from each global skills dir to looper/skills/{name}.
// Returns the number of links created/updated.
func (m *SkillManager) EnsureSymlinks() (int, error) {
	globalDirs := m.snapshotGlobalDirs()
	if len(globalDirs) == 0 {
		return 0, fmt.Errorf("no global skill dirs configured")
	}

	entries, err := os.ReadDir(m.skillsDir)
	if err != nil {
		return 0, fmt.Errorf("read skills dir: %w", err)
	}

	updated := 0
	for _, globalDir := range globalDirs {
		if err := os.MkdirAll(globalDir, 0o755); err != nil {
			slog.Warn("create global skills dir failed", "dir", globalDir, "error", err)
			continue
		}

		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				continue
			}

			src := filepath.Join(m.skillsDir, name)
			dst := filepath.Join(globalDir, name)

			// Check if symlink already points to correct target
			if target, err := os.Readlink(dst); err == nil {
				if target == src {
					continue // already correct
				}
				// Wrong target, remove and recreate
				os.Remove(dst)
			} else if _, err := os.Lstat(dst); err == nil {
				// Exists but not a symlink (real dir) — skip to avoid data loss
				slog.Warn("skill exists as real directory, skipping symlink", "name", name, "path", dst)
				continue
			}

			if err := os.Symlink(src, dst); err != nil {
				slog.Error("create symlink failed", "name", name, "target_dir", globalDir, "error", err)
				continue
			}
			slog.Info("skill symlink created", "name", name, "target", src, "link", dst)
			updated++
		}
	}
	return updated, nil
}

// GenerateLock computes SHA256 hashes for all skill files and writes skills-lock.json.
func (m *SkillManager) GenerateLock() error {
	entries, err := os.ReadDir(m.skillsDir)
	if err != nil {
		return fmt.Errorf("read skills dir: %w", err)
	}

	lock := SkillsLock{
		Version: 1,
		Skills:  make(map[string]SkillEntry),
	}

	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
			continue
		}

		entry, err := m.hashSkill(filepath.Join(m.skillsDir, e.Name()))
		if err != nil {
			slog.Warn("hash skill failed", "name", e.Name(), "error", err)
			continue
		}
		lock.Skills[e.Name()] = entry
	}

	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal lock: %w", err)
	}
	data = append(data, '\n')

	return os.WriteFile(m.lockPath, data, 0o644)
}

// ValidateSkill checks that a skill exists and has a SKILL.md file.
func (m *SkillManager) ValidateSkill(name string) error {
	skillDir := filepath.Join(m.skillsDir, name)
	if _, err := os.Stat(skillDir); err != nil {
		return fmt.Errorf("skill directory not found: %s", name)
	}
	skillFile := filepath.Join(skillDir, "SKILL.md")
	if _, err := os.Stat(skillFile); err != nil {
		return fmt.Errorf("SKILL.md not found for skill: %s", name)
	}
	return nil
}

func (m *SkillManager) SkillFilePath(name string) string {
	return filepath.Join(m.skillsDir, name, "SKILL.md")
}

func (m *SkillManager) LoadSkillContent(name string) (string, error) {
	if err := m.ValidateSkill(name); err != nil {
		return "", err
	}
	data, err := os.ReadFile(m.SkillFilePath(name))
	if err != nil {
		return "", fmt.Errorf("read skill file: %w", err)
	}
	return string(data), nil
}

// VerifyLock checks current skill hashes against the lock file.
// Returns a list of skill names that have changed.
func (m *SkillManager) VerifyLock() ([]string, error) {
	data, err := os.ReadFile(m.lockPath)
	if err != nil {
		return nil, fmt.Errorf("read lock file: %w", err)
	}

	var lock SkillsLock
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("parse lock file: %w", err)
	}

	var changed []string
	for name, locked := range lock.Skills {
		current, err := m.hashSkill(filepath.Join(m.skillsDir, name))
		if err != nil {
			changed = append(changed, name+" (missing)")
			continue
		}
		if current.Hash != locked.Hash {
			changed = append(changed, name)
		}
	}
	return changed, nil
}

// hashSkill computes a composite hash for a skill directory.
func (m *SkillManager) hashSkill(dir string) (SkillEntry, error) {
	var entry SkillEntry
	entry.Files = make(map[string]string)

	h := sha256.New()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fileHash := fmt.Sprintf("%x", sha256.Sum256(data))
		entry.Files[rel] = fileHash
		h.Write([]byte(rel))
		h.Write(data)
		return nil
	})
	if err != nil {
		return entry, err
	}
	entry.Hash = fmt.Sprintf("%x", h.Sum(nil))
	return entry, nil
}
