package accountpolicy

// SetObservationObserver connects an independent archive. The callback must not
// return errors into the routing state machine and runs outside its main lock.
func (s *Service) SetObservationObserver(f func(Snapshot, Snapshot, []Operation)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.Observe = f
}
func (s *Service) notifyObservation(old, next Snapshot, ops []Operation) {
	s.mu.RLock()
	f := s.opts.Observe
	enabled := s.state.Settings.UsageHistoryEnabled
	s.mu.RUnlock()
	if f != nil && enabled {
		f(old, next, ops)
	}
}
