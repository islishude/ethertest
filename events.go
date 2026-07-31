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
	BlockHash   common.Hash `json:"block_hash,omitempty"`
	BlockNumber uint64      `json:"block_number,omitempty"`
	Removed     bool        `json:"removed,omitempty"`
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

func (l *eventLog) append(event Event) (Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	event.Revision = l.next
	encoded, err := json.Marshal(event)
	if err != nil {
		return Event{}, err
	}
	var next [8]byte
	binary.BigEndian.PutUint64(next[:], uint64(l.next+1))
	batch := l.db.NewBatch()
	if err := batch.Put(eventKey(event.Revision), encoded); err != nil {
		return Event{}, err
	}
	if err := batch.Put(eventNextKey, next[:]); err != nil {
		return Event{}, err
	}
	var removed *Event
	if uint64(len(l.events)) > l.capacity {
		return Event{}, errors.New("event log capacity invariant violated")
	}
	if uint64(len(l.events)) == l.capacity {
		oldest := l.events[0]
		removed = &oldest
		if err := batch.Delete(eventKey(oldest.Revision)); err != nil {
			return Event{}, err
		}
	}
	if err := batch.Write(); err != nil {
		return Event{}, err
	}
	l.next++
	l.events = append(l.events, event)
	if removed != nil {
		l.events = l.events[1:]
	}
	close(l.changed)
	l.changed = make(chan struct{})
	return event, nil
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
