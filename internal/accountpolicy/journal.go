package accountpolicy

import (
	"encoding/json"
	"os"
	"path/filepath"
)

func (s *Service) reloadOperations() error {
	data, err := os.ReadFile(filepath.Join(s.stateDir, "state.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return policyError("persistence", "cannot reload operation journal")
	}
	var disk storedState
	if json.Unmarshal(data, &disk) != nil || disk.Version != 1 {
		return policyError("persistence", "invalid operation journal")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Operations = disk.Operations
	return nil
}
