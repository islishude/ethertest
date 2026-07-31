package ethertest

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
)

var ErrEventGap = errors.New("requested revision is no longer available")

type Revision uint64

type Event struct {
	Revision    Revision    `json:"revision"`
	Type        string      `json:"type"`
	Slot        uint64      `json:"slot"`
	BlockHash   common.Hash `json:"block_hash,omitempty"`
	BlockNumber uint64      `json:"block_number,omitempty"`
	Removed     bool        `json:"removed,omitempty"`
	OldHead     common.Hash `json:"old_head,omitempty"`
	NewHead     common.Hash `json:"new_head,omitempty"`
	Depth       uint64      `json:"depth,omitempty"`
}

type eventPlan struct {
	events  []Event
	puts    []journalKV
	deletes [][]byte
	next    Revision
}

type eventLog struct {
	mu       sync.RWMutex
	capacity uint64
	next     Revision
	events   []Event
	db       ethdb.Database
	changed  chan struct{}
}

var (
	eventNamespace = []byte("ethertest/event/")
	eventNextKey   = []byte("ethertest/meta/event-next")
)

func newEventLog(capacity uint64, db ethdb.Database) (*eventLog, error) {
	log := &eventLog{capacity: capacity, next: 1, db: db, changed: make(chan struct{})}
	iterator := db.NewIterator(eventNamespace, nil)
	defer iterator.Release()
	for iterator.Next() {
		var event Event
		if err := json.Unmarshal(iterator.Value(), &event); err != nil {
			return nil, err
		}
		log.events = append(log.events, event)
		if event.Revision >= log.next {
			log.next = event.Revision + 1
		}
	}
	if err := iterator.Error(); err != nil {
		return nil, err
	}
	return log, nil
}

func (l *eventLog) plan(events []Event) (eventPlan, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	plan := eventPlan{events: append([]Event(nil), events...), next: l.next}
	for index := range plan.events {
		plan.events[index].Revision = plan.next
		encoded, err := json.Marshal(plan.events[index])
		if err != nil {
			return eventPlan{}, err
		}
		plan.puts = append(plan.puts, journalKV{Key: eventKey(plan.next), Value: encoded})
		plan.next++
	}
	var next [8]byte
	binary.BigEndian.PutUint64(next[:], uint64(plan.next))
	plan.puts = append(plan.puts, journalKV{Key: append([]byte(nil), eventNextKey...), Value: next[:]})
	retained := append(append([]Event(nil), l.events...), plan.events...)
	if uint64(len(retained)) > l.capacity {
		remove := len(retained) - int(l.capacity)
		for _, event := range retained[:remove] {
			plan.deletes = append(plan.deletes, eventKey(event.Revision))
		}
	}
	return plan, nil
}

func (l *eventLog) apply(plan eventPlan) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(plan.events) == 0 {
		return
	}
	l.next = plan.next
	l.events = append(l.events, plan.events...)
	if uint64(len(l.events)) > l.capacity {
		l.events = l.events[len(l.events)-int(l.capacity):]
	}
	close(l.changed)
	l.changed = make(chan struct{})
}

func eventKey(revision Revision) []byte {
	key := make([]byte, len(eventNamespace)+8)
	copy(key, eventNamespace)
	binary.BigEndian.PutUint64(key[len(eventNamespace):], uint64(revision))
	return key
}

func (l *eventLog) since(revision Revision) ([]Event, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.sinceLocked(revision)
}

func (l *eventLog) sinceAndWait(revision Revision) ([]Event, <-chan struct{}, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	events, err := l.sinceLocked(revision)
	return events, l.changed, err
}

func (l *eventLog) sinceLocked(revision Revision) ([]Event, error) {
	if len(l.events) == 0 {
		return nil, nil
	}
	if revision+1 < l.events[0].Revision {
		return nil, ErrEventGap
	}
	index := 0
	for index < len(l.events) && l.events[index].Revision <= revision {
		index++
	}
	return append([]Event(nil), l.events[index:]...), nil
}

func (l *eventLog) current() Revision {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.next - 1
}
