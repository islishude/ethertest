package ethertest

import (
	"bytes"
	"encoding/json"
	"sync"

	"github.com/ethereum/go-ethereum/common"
)

type pendingHashEvent struct {
	Sequence    uint64
	Hash        common.Hash
	Transaction json.RawMessage
}

type pendingHashLog struct {
	mu       sync.RWMutex
	capacity uint64
	next     uint64
	events   []pendingHashEvent
	changed  chan struct{}
}

func newPendingHashLog(capacity uint64) *pendingHashLog {
	return &pendingHashLog{capacity: capacity, next: 1, changed: make(chan struct{})}
}

func (log *pendingHashLog) record(hash common.Hash, transaction json.RawMessage) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.events = append(log.events, pendingHashEvent{Sequence: log.next, Hash: hash, Transaction: bytes.Clone(transaction)})
	log.next++
	close(log.changed)
	log.changed = make(chan struct{})
	if uint64(len(log.events)) > log.capacity {
		log.events = log.events[len(log.events)-int(log.capacity):]
	}
}

func (log *pendingHashLog) current() uint64 {
	log.mu.RLock()
	defer log.mu.RUnlock()
	return log.next - 1
}

func (log *pendingHashLog) since(sequence uint64) ([]pendingHashEvent, error) {
	log.mu.RLock()
	defer log.mu.RUnlock()
	return log.sinceLocked(sequence)
}

func (log *pendingHashLog) sinceAndWait(sequence uint64) ([]pendingHashEvent, <-chan struct{}, error) {
	log.mu.RLock()
	defer log.mu.RUnlock()
	events, err := log.sinceLocked(sequence)
	return events, log.changed, err
}

func (log *pendingHashLog) sinceLocked(sequence uint64) ([]pendingHashEvent, error) {
	if len(log.events) == 0 {
		return nil, nil
	}
	if sequence+1 < log.events[0].Sequence {
		return nil, ErrEventGap
	}
	index := 0
	for index < len(log.events) && log.events[index].Sequence <= sequence {
		index++
	}
	return append([]pendingHashEvent(nil), log.events[index:]...), nil
}
