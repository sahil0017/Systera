package outbox

import "sync"

// Notifier fans out "run has new events" signals to in-process subscribers.
type Notifier struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func NewNotifier() *Notifier {
	return &Notifier{subs: make(map[string]map[chan struct{}]struct{})}
}

// Subscribe registers interest in runID. The returned cancel func must be called.
func (n *Notifier) Subscribe(runID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	if n.subs[runID] == nil {
		n.subs[runID] = make(map[chan struct{}]struct{})
	}
	n.subs[runID][ch] = struct{}{}
	n.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			n.mu.Lock()
			delete(n.subs[runID], ch)
			if len(n.subs[runID]) == 0 {
				delete(n.subs, runID)
			}
			n.mu.Unlock()
		})
	}
}

// Notify wakes every subscriber of runID without blocking.
func (n *Notifier) Notify(runID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for ch := range n.subs[runID] {
		select {
		case ch <- struct{}{}:
		default: // a wake-up is already pending
		}
	}
}

// NotifyAll wakes every subscriber (used after a listener reconnect, when notifications
// may have been missed).
func (n *Notifier) NotifyAll() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, set := range n.subs {
		for ch := range set {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}
