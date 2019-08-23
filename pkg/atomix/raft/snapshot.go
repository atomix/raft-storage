// Copyright 2019-present Open Networking Foundation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import (
	"bytes"
	"io"
	"time"
)

type SnapshotStore interface {
	newSnapshot(index Index, timestamp time.Time) Snapshot
	CurrentSnapshot() Snapshot
}

type Snapshot interface {
	Index() Index
	Timestamp() time.Time
	Reader() io.ReadCloser
	Writer() io.WriteCloser
}

func newMemorySnapshotStore() SnapshotStore {
	return &memorySnapshotStore{
		snapshots: make(map[Index]Snapshot),
	}
}

type memorySnapshotStore struct {
	snapshots       map[Index]Snapshot
	currentSnapshot Snapshot
}

func (s *memorySnapshotStore) newSnapshot(index Index, timestamp time.Time) Snapshot {
	snapshot := &memorySnapshot{
		index:     index,
		timestamp: timestamp,
		bytes:     make([]byte, 0, 1024*1024),
	}
	s.snapshots[index] = snapshot
	s.currentSnapshot = snapshot
	return snapshot
}

func (s *memorySnapshotStore) CurrentSnapshot() Snapshot {
	return s.currentSnapshot
}

type memorySnapshot struct {
	index     Index
	timestamp time.Time
	bytes     []byte
}

func (s *memorySnapshot) Index() Index {
	return s.index
}

func (s *memorySnapshot) Timestamp() time.Time {
	return s.timestamp
}

func (s *memorySnapshot) Reader() io.ReadCloser {
	return &memoryReader{
		reader: bytes.NewReader(s.bytes),
	}
}

func (s *memorySnapshot) Writer() io.WriteCloser {
	return &memoryWriter{
		writer: bytes.NewBuffer(s.bytes),
	}
}

type memoryReader struct {
	reader io.Reader
}

func (r *memoryReader) Read(p []byte) (n int, err error) {
	return r.reader.Read(p)
}

func (r *memoryReader) Close() error {
	return nil
}

type memoryWriter struct {
	writer io.Writer
}

func (w *memoryWriter) Write(p []byte) (n int, err error) {
	return w.writer.Write(p)
}

func (w *memoryWriter) Close() error {
	return nil
}
