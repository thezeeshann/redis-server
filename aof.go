package main

import (
	"io"
	"os"
	"sync"
	"time"
)

// Aof is an append-only log of every state-changing command the server has
// executed. Replaying it from the top rebuilds the dataset exactly.
type Aof struct {
	file *os.File
	mu   sync.Mutex
	done chan struct{}
}

func NewAof(path string) (*Aof, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return nil, err
	}

	aof := &Aof{
		file: f,
		done: make(chan struct{}),
	}

	// Flush the OS write buffer to disk once a second, so a crash loses at
	// most one second of writes instead of everything still sitting in cache.
	go aof.syncEvery(time.Second)

	return aof, nil
}

func (aof *Aof) syncEvery(d time.Duration) {
	ticker := time.NewTicker(d)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			aof.mu.Lock()
			aof.file.Sync()
			aof.mu.Unlock()
		case <-aof.done:
			return
		}
	}
}

func (aof *Aof) Close() error {
	// Stop the syncer first, otherwise it keeps calling Sync on a closed file.
	close(aof.done)

	aof.mu.Lock()
	defer aof.mu.Unlock()

	if err := aof.file.Sync(); err != nil {
		return err
	}

	return aof.file.Close()
}

// Write appends a command to the log in RESP form.
func (aof *Aof) Write(value Value) error {
	aof.mu.Lock()
	defer aof.mu.Unlock()

	_, err := aof.file.Write(value.Marshal())

	return err
}

// Read replays the log from the beginning, handing each stored command to
// callback. It leaves the file offset at the end so later writes append.
func (aof *Aof) Read(callback func(value Value)) error {
	aof.mu.Lock()
	defer aof.mu.Unlock()

	if _, err := aof.file.Seek(0, io.SeekStart); err != nil {
		return err
	}

	resp := NewResp(aof.file)

	for {
		value, err := resp.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		callback(value)
	}

	// bufio may have read past what it handed back, so the descriptor offset
	// is unreliable. Reset it to the true end before any append happens.
	_, err := aof.file.Seek(0, io.SeekEnd)

	return err
}
