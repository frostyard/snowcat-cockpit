package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

const stateFilename = "node.json"

var nodeIDPattern = regexp.MustCompile(`^node-[0-9a-f]{32}$`)

type Node struct {
	NodeID    string    `json:"nodeId"`
	CreatedAt time.Time `json:"createdAt"`
}

func Open(directory string) (Node, error) {
	if directory == "" {
		return Node{}, errors.New("state directory must not be empty")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return Node{}, fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return Node{}, fmt.Errorf("secure state directory: %w", err)
	}

	path := filepath.Join(directory, stateFilename)
	node, err := read(path)
	if err == nil {
		if err := os.Chmod(path, 0o600); err != nil {
			return Node{}, fmt.Errorf("secure node state: %w", err)
		}
		return node, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Node{}, err
	}

	nodeID, err := newNodeID()
	if err != nil {
		return Node{}, err
	}
	node = Node{NodeID: nodeID, CreatedAt: time.Now().UTC()}
	if err := writeAtomic(path, node); err != nil {
		return Node{}, err
	}
	return node, nil
}

func read(path string) (Node, error) {
	file, err := os.Open(path)
	if err != nil {
		return Node{}, err
	}
	defer file.Close()

	decoder := json.NewDecoder(io.LimitReader(file, 64*1024))
	decoder.DisallowUnknownFields()
	var node Node
	if err := decoder.Decode(&node); err != nil {
		return Node{}, fmt.Errorf("decode node state: %w", err)
	}
	if !nodeIDPattern.MatchString(node.NodeID) {
		return Node{}, errors.New("decode node state: invalid node ID")
	}
	if node.CreatedAt.IsZero() {
		return Node{}, errors.New("decode node state: missing creation time")
	}
	return node, nil
}

func newNodeID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("create node ID: %w", err)
	}
	return "node-" + hex.EncodeToString(bytes), nil
}

func writeAtomic(path string, node Node) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".node-*.json")
	if err != nil {
		return fmt.Errorf("create temporary node state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()

	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary node state: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(node); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("encode node state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync node state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close node state: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install node state: %w", err)
	}
	return nil
}
