package navgraph

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"

	"github.com/onehumanbeing/esp32-iphone-agent/cli/internal/history"
	"github.com/onehumanbeing/esp32-iphone-agent/cli/internal/intent"
)

const maxImageBytes = 16 << 20

// SaveScreenshot copies a JPEG/PNG into the graph's managed image folder. The DB stores only metadata.
func (s *Store) SaveScreenshot(node, path string) (Screenshot, error) {
	if _, err := s.Node(node); err != nil {
		return Screenshot{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return Screenshot{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxImageBytes+1))
	if err != nil {
		return Screenshot{}, err
	}
	if len(data) > maxImageBytes {
		return Screenshot{}, errors.New("reference screenshot exceeds 16 MiB")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Screenshot{}, fmt.Errorf("reference screenshot: %w", err)
	}
	if format != "jpeg" && format != "png" {
		return Screenshot{}, errors.New("reference screenshot must be JPEG or PNG")
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 16384 || cfg.Height > 16384 {
		return Screenshot{}, errors.New("invalid reference screenshot dimensions")
	}
	sum := sha256.Sum256(data)
	rel := filepath.Join("screenshots", hex.EncodeToString(sum[:])+"."+format)
	if err = os.MkdirAll(filepath.Join(s.dir, "screenshots"), 0o700); err != nil {
		return Screenshot{}, err
	}
	// Atomic publication prevents a concurrent writer from exposing a truncated image.
	tmp, err := os.CreateTemp(filepath.Join(s.dir, "screenshots"), ".image-*")
	if err != nil {
		return Screenshot{}, err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return Screenshot{}, err
	}
	if err = tmp.Close(); err != nil {
		return Screenshot{}, err
	}
	if err = os.Rename(tmp.Name(), filepath.Join(s.dir, rel)); err != nil {
		// Windows cannot rename over an existing file; the content hash makes a
		// previously published copy equivalent. Never ignore another rename error.
		existing, readErr := os.ReadFile(filepath.Join(s.dir, rel))
		if readErr != nil || !bytes.Equal(existing, data) {
			return Screenshot{}, err
		}
	}
	shot := Screenshot{ID: intent.NewID(), Node: node, Path: rel, Width: cfg.Width, Height: cfg.Height, CreatedAt: s.now().UnixMilli()}
	_, err = s.db.Exec("INSERT INTO screenshots(id,node,path,width,height,created_at) VALUES(?,?,?,?,?,?)", shot.ID, shot.Node, shot.Path, shot.Width, shot.Height, shot.CreatedAt)
	if err != nil {
		return Screenshot{}, err
	}
	return s.Screenshot(shot.ID)
}

func (s *Store) Screenshot(id string) (Screenshot, error) {
	var shot Screenshot
	err := s.db.QueryRow("SELECT id,node,path,width,height,created_at FROM screenshots WHERE id=?", id).Scan(&shot.ID, &shot.Node, &shot.Path, &shot.Width, &shot.Height, &shot.CreatedAt)
	if err == nil {
		shot.Path = filepath.Join(s.dir, shot.Path)
	}
	return shot, err
}

func (s *Store) Screenshots(node string, limit, offset int) ([]Screenshot, error) {
	if _, err := s.Node(node); err != nil {
		return nil, err
	}
	if err := page(limit, offset); err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT id,node,path,width,height,created_at FROM screenshots WHERE node=? ORDER BY created_at DESC,rowid DESC LIMIT ? OFFSET ?", node, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Screenshot{}
	for rows.Next() {
		var shot Screenshot
		if err = rows.Scan(&shot.ID, &shot.Node, &shot.Path, &shot.Width, &shot.Height, &shot.CreatedAt); err != nil {
			return nil, err
		}
		shot.Path = filepath.Join(s.dir, shot.Path)
		out = append(out, shot)
	}
	return out, rows.Err()
}

type SavedEdge struct {
	Edge      Edge        `json:"edge"`
	Reference *Screenshot `json:"reference,omitempty"`
}

// SaveHistoryEdge copies the action and source-page reference, NOT an assertion of success.
// history.Step.Replayable means command delivery worked, not that the destination was reached.
func (s *Store) SaveHistoryEdge(configDir string, in EdgeInput) (SavedEdge, error) {
	if in.FromRun == "" {
		if in.Step != 0 {
			return SavedEdge{}, errors.New("step requires from_run")
		}
		e, err := s.SaveEdge(in)
		return SavedEdge{Edge: e}, err
	}
	if len(in.Action) > 0 {
		return SavedEdge{}, errors.New("provide either action or from_run/step")
	}
	run, err := history.Load(history.Root(configDir), in.FromRun)
	if err != nil {
		return SavedEdge{}, err
	}
	if in.Step < 1 || in.Step > len(run.Steps) {
		return SavedEdge{}, errors.New("step must be a valid 1-based history action number")
	}
	st := run.Steps[in.Step-1]
	if st.Error != "" {
		return SavedEdge{}, errors.New("cannot save an action that returned an error")
	}
	in.Action, in.FromRun = Action(st.Action), run.ID
	if err = in.Action.Validate(); err != nil {
		return SavedEdge{}, err
	}
	e, err := s.SaveEdge(in)
	if err != nil {
		return SavedEdge{}, err
	}
	out := SavedEdge{Edge: e}
	if st.Frame != "" {
		// A run may already have lost its reference file. Keep the edge usable and report this as an error.
		shot, err := s.SaveScreenshot(in.From, filepath.Join(run.Dir, filepath.Base(st.Frame)))
		if err != nil {
			return out, fmt.Errorf("edge %s saved but reference was not copied: %w", e.ID, err)
		}
		out.Reference = &shot
	}
	return out, nil
}
