package raft

import (
	"container/list"
	"context"
	"errors"
	log "github.com/sirupsen/logrus"
	"math"
	"sort"
	"sync"
	"time"
)

// newAppender returns a new appender
func newAppender(server *RaftServer) *raftAppender {
	commitCh := make(chan memberCommit)
	failCh := make(chan time.Time)
	members := make(map[string]*memberAppender)
	for _, member := range server.cluster.members {
		if member.MemberId != server.cluster.member {
			members[member.MemberId] = newMemberAppender(server, member, commitCh, failCh)
		}
	}
	appender := &raftAppender{
		server:           server,
		members:          members,
		commitIndexes:    make(map[string]int64),
		commitTimes:      make(map[string]time.Time),
		heartbeatFutures: list.New(),
		commitChannels:   make(map[int64]chan int64),
		commitCh:         commitCh,
		failCh:           failCh,
		lastQuorumTime:   time.Now(),
		stopped:          make(chan bool),
	}
	return appender
}

// raftAppender handles replication on the leader
type raftAppender struct {
	server           *RaftServer
	members          map[string]*memberAppender
	commitIndexes    map[string]int64
	commitTimes      map[string]time.Time
	heartbeatFutures *list.List
	commitChannels   map[int64]chan int64
	commitCh         chan memberCommit
	failCh           chan time.Time
	stopped          chan bool
	lastQuorumTime   time.Time
	mu               sync.Mutex
}

// start starts the appender
func (a *raftAppender) start() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, member := range a.members {
		go member.start()
	}
	a.processCommits()
}

// heartbeat sends a heartbeat to a majority of followers
func (a *raftAppender) heartbeat() error {
	// If there are no members to send the entry to, immediately return.
	if len(a.members) == 0 {
		return nil
	}

	ch := make(chan int64)
	future := heartbeatFuture{}

	// Acquire a lock to add the future to the heartbeat futures.
	a.mu.Lock()
	a.heartbeatFutures.PushBack(future)
	a.mu.Unlock()

	// Iterate through member appenders and add the future time to the heartbeat channels.
	for _, member := range a.members {
		member.heartbeatCh <- future.time
	}
	_, ok := <-ch
	if ok {
		return nil
	} else {
		return errors.New("failed to verify quorum")
	}
}

// append replicates the given entry to all followers
func (a *raftAppender) append(entry *IndexedEntry) error {
	// If there are no members to send the entry to, immediately commit it.
	if len(a.members) == 0 {
		a.server.writeLock()
		a.server.setCommitIndex(entry.Index)
		a.server.writeUnlock()
		return nil
	}

	ch := make(chan int64)
	a.commitChannels[entry.Index] = ch
	for _, member := range a.members {
		member.entryCh <- entry
	}
	_, ok := <-ch
	if ok {
		return nil
	} else {
		return errors.New("failed to commit entry")
	}
}

// processCommits handles member commit events and updates the local commit index
func (a *raftAppender) processCommits() {
	for {
		select {
		case commit := <-a.commitCh:
			a.commit(commit.member, commit.index, commit.time)
		case failTime := <-a.failCh:
			a.failTime(failTime)
		case <-a.stopped:
			return
		}
	}
}

func (a *raftAppender) commit(member *memberAppender, index int64, time time.Time) {
	if !member.active {
		return
	}
	a.commitIndex(member.member.MemberId, index)
	a.commitTime(member.member.MemberId, time)
}

func (a *raftAppender) commitIndex(member string, index int64) {
	prevIndex := a.commitIndexes[member]
	if index > prevIndex {
		a.commitIndexes[member] = index

		indexes := make([]int64, len(a.members))
		i := 0
		for _, index := range a.commitIndexes {
			indexes[i] = index
			i++
		}
		sort.Slice(indexes, func(i, j int) bool {
			return indexes[i] < indexes[j]
		})

		// Acquire a write lock to increment the commitIndex.
		a.server.writeLock()
		defer a.server.writeUnlock()

		commitIndex := indexes[len(a.members)/2]
		for i := a.server.commitIndex + 1; i <= commitIndex; i++ {
			a.server.setCommitIndex(i)
			ch, ok := a.commitChannels[i]
			if ok {
				ch <- i
			}
		}
	}
}

func (a *raftAppender) commitTime(member string, time time.Time) {
	prevTime := a.commitTimes[member]
	nextTime := time
	if nextTime.UnixNano() > prevTime.UnixNano() {
		a.commitTimes[member] = nextTime

		times := make([]int64, len(a.members))
		i := 0
		for _, time := range a.commitTimes {
			times[i] = time.UnixNano()
			i++
		}
		sort.Slice(times, func(i, j int) bool {
			return times[i] < times[j]
		})

		commitTime := times[len(a.members)/2]
		a.mu.Lock()
		for commitFuture := a.heartbeatFutures.Front(); commitFuture != nil && commitFuture.Value.(heartbeatFuture).time.UnixNano() < commitTime; commitFuture = a.heartbeatFutures.Front() {
			ch := commitFuture.Value.(heartbeatFuture).ch
			ch <- struct{}{}
			close(ch)
			a.heartbeatFutures.Remove(commitFuture)
		}
		a.mu.Unlock()

		// Update the last time a quorum of the cluster was reached
		a.lastQuorumTime = time
	}
}

func (a *raftAppender) failTime(failTime time.Time) {
	if failTime.Sub(a.lastQuorumTime) > a.server.electionTimeout*2 {
		log.WithField("memberID", a.server.cluster.member).
			Warn("Suspected network partition; stepping down")
		a.server.setLeader("")
		a.server.becomeFollower()
	}
}

func (a *raftAppender) stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, member := range a.members {
		member.stop()
	}
	a.stopped <- true
}

// heartbeatFuture is a heartbeat channel with a timestamp indicating when the heartbeat was requested
type heartbeatFuture struct {
	ch   chan struct{}
	time time.Time
}

// memberCommit is an event carrying the match index for a member
type memberCommit struct {
	member *memberAppender
	index  int64
	time   time.Time
}

const (
	minBackoffFailureCount = 5
	maxHeartbeatWait       = 1 * time.Minute
	maxBatchSize           = 1024 * 1024
)

func newMemberAppender(server *RaftServer, member *RaftMember, commitCh chan<- memberCommit, failCh chan<- time.Time) *memberAppender {
	ticker := time.NewTicker(server.electionTimeout / 2)
	reader := server.log.OpenReader(0)
	return &memberAppender{
		server:      server,
		member:      member,
		nextIndex:   reader.LastIndex() + 1,
		entryCh:     make(chan *IndexedEntry),
		appendCh:    make(chan int64),
		commitCh:    commitCh,
		failCh:      failCh,
		heartbeatCh: make(chan time.Time),
		stopped:     make(chan bool),
		reader:      reader,
		tickTicker:  ticker,
		tickCh:      ticker.C,
		queue:       list.New(),
	}
}

// memberAppender handles replication to a member
type memberAppender struct {
	server            *RaftServer
	member            *RaftMember
	active            bool
	snapshotIndex     int64
	prevTerm          int64
	nextIndex         int64
	matchIndex        int64
	lastHeartbeatTime time.Time
	lastResponseTime  time.Time
	appending         bool
	failureCount      int
	firstFailureTime  time.Time
	entryCh           chan *IndexedEntry
	appendCh          chan int64
	commitCh          chan<- memberCommit
	failCh            chan<- time.Time
	heartbeatCh       chan time.Time
	tickCh            <-chan time.Time
	tickTicker        *time.Ticker
	stopped           chan bool
	reader            RaftLogReader
	queue             *list.List
}

// start starts sending append requests to the member
func (a *memberAppender) start() {
	a.active = true
	a.processEvents()
}

func (a *memberAppender) processEvents() {
	for {
		select {
		case entry := <-a.entryCh:
			if a.failureCount == 0 {
				a.queue.PushBack(entry)
			}
			if !a.appending {
				a.appending = true
				go a.append()
			}
		case nextIndex := <-a.appendCh:
			a.appending = false
			if a.reader.LastIndex() >= nextIndex {
				a.appending = true
				go a.append()
			}
		case <-a.heartbeatCh:
			go a.sendAppendRequest(a.emptyAppendRequest())
		case <-a.tickCh:
			if !a.appending {
				a.appending = true
				go a.append()
			}
		case <-a.stopped:
			return
		}
	}
}

func (a *memberAppender) append() {
	if a.failureCount >= minBackoffFailureCount {
		timeSinceFailure := float64(time.Now().Sub(a.firstFailureTime))
		heartbeatWaitTime := math.Min(float64(a.failureCount)*float64(a.failureCount)*float64(a.server.electionTimeout), float64(maxHeartbeatWait))
		if timeSinceFailure > heartbeatWaitTime {
			a.sendAppendRequest(a.nextAppendRequest())
		}
	} else {
		// TODO: The snapshot store needs concurrency control when accessing the snapshots for replication.
		snapshot := a.server.snapshot.CurrentSnapshot()
		if snapshot != nil && a.snapshotIndex < snapshot.Index() && snapshot.Index() >= a.nextIndex {
			log.WithField("memberID", a.server.cluster.member).
				Debugf("Replicating snapshot %d to %s", snapshot.Index(), a.member.MemberId)
			a.sendInstallRequests(snapshot)
		} else {
			a.sendAppendRequest(a.nextAppendRequest())
		}
	}
}

// stop stops sending append requests to the member
func (a *memberAppender) stop() {
	a.active = false
	close(a.entryCh)
	close(a.appendCh)
	close(a.heartbeatCh)
	a.tickTicker.Stop()
	a.stopped <- true
}

func (a *memberAppender) succeed() {
	a.failureCount = 0
}

func (a *memberAppender) fail(time time.Time) {
	if a.failureCount == 0 {
		a.firstFailureTime = time
	}
	a.failureCount++
	a.failCh <- time
}

func (a *memberAppender) requeue() {
	a.appendCh <- a.nextIndex
}

func (a *memberAppender) newInstallRequest(snapshot Snapshot, bytes []byte) *InstallRequest {
	a.server.readLock()
	defer a.server.readUnlock()
	return &InstallRequest{
		Term:      a.server.term,
		Leader:    a.server.leader,
		Index:     snapshot.Index(),
		Timestamp: snapshot.Timestamp(),
		Data:      bytes,
	}
}

func (a *memberAppender) sendInstallRequests(snapshot Snapshot) {
	// Start the append to the member.
	startTime := time.Now()

	client, err := a.server.cluster.getClient(a.member.MemberId)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), a.server.electionTimeout)
	defer cancel()

	stream, err := client.Install(ctx)
	if err != nil {
		a.handleInstallError(snapshot, err, startTime)
		return
	}

	reader := snapshot.Reader()
	defer reader.Close()
	bytes := make([]byte, maxBatchSize)
	n, err := reader.Read(bytes)
	for n > 0 && err == nil {
		request := a.newInstallRequest(snapshot, bytes[:n])
		a.server.logSendTo("InstallRequest", request, a.member.MemberId)
		stream.Send(request)
		n, err = reader.Read(bytes)
	}
	if err != nil {
		log.WithField("memberID", a.server.cluster.member).
			Warn("Failed to read snapshot", err)
	}

	response, err := stream.CloseAndRecv()
	if err == nil {
		a.server.logReceiveFrom("InstallResponse", response, a.member.MemberId)
		if response.Status == ResponseStatus_OK {
			a.handleInstallResponse(snapshot, response, startTime)
		} else {
			a.handleInstallFailure(snapshot, response, startTime)
		}
	} else {
		a.server.logErrorFrom("InstallRequest", err, a.member.MemberId)
		a.handleInstallError(snapshot, err, startTime)
	}
}

func (a *memberAppender) handleInstallResponse(snapshot Snapshot, response *InstallResponse, startTime time.Time) {
	// Reset the member failure count to allow entries to be sent to the member.
	a.succeed()

	// Update the snapshot index
	a.snapshotIndex = snapshot.Index()

	// Send a commit event to the parent appender.
	a.commit(startTime)

	// Requeue the append for the nextIndex.
	a.requeue()
}

func (a *memberAppender) handleInstallFailure(snapshot Snapshot, response *InstallResponse, startTime time.Time) {
	// In the event of an install response error, simply do nothing and await the next heartbeat.
	// This prevents infinite loops when installation fails.
}

func (a *memberAppender) handleInstallError(snapshot Snapshot, err error, startTime time.Time) {
	log.WithField("memberID", a.server.cluster.member).
		Debugf("Failed to install %s: %s", a.member.MemberId, err)
	a.server.cluster.resetClient(a.member.MemberId)
	a.fail(startTime)
	a.requeue()
}

func (a *memberAppender) nextAppendRequest() *AppendRequest {
	// If the log is empty then send an empty commit.
	// If the next index hasn't yet been set then we send an empty commit first.
	// If the next index is greater than the last index then send an empty commit.
	// If the member failed to respond to recent communication send an empty commit. This
	// helps avoid doing expensive work until we can ascertain the member is back up.
	a.server.readLock()
	defer a.server.readUnlock()
	if a.failureCount > 0 || a.reader.CurrentIndex() == a.reader.LastIndex() {
		return a.emptyAppendRequest()
	} else {
		return a.entriesAppendRequest()
	}
}

func (a *memberAppender) emptyAppendRequest() *AppendRequest {
	return &AppendRequest{
		Term:         a.server.term,
		Leader:       a.server.leader,
		PrevLogIndex: a.nextIndex - 1,
		PrevLogTerm:  a.prevTerm,
		CommitIndex:  a.server.commitIndex,
	}
}

func (a *memberAppender) entriesAppendRequest() *AppendRequest {
	request := &AppendRequest{
		Term:         a.server.term,
		Leader:       a.server.leader,
		PrevLogIndex: a.nextIndex - 1,
		PrevLogTerm:  a.prevTerm,
		CommitIndex:  a.server.commitIndex,
	}

	entriesList := list.New()

	// Build a list of entries starting at the nextIndex, using the cache if possible.
	size := 0
	nextIndex := a.nextIndex
	for nextIndex <= a.reader.LastIndex() {
		// First, try to get the entry from the cache.
		entry := a.queue.Front()
		if entry != nil {
			indexed := entry.Value.(*IndexedEntry)
			if indexed.Index == nextIndex {
				entriesList.PushBack(indexed.Entry)
				a.queue.Remove(entry)
				size += indexed.Entry.XXX_Size()
				nextIndex++
				if size >= maxBatchSize {
					break
				}
				continue
			} else if indexed.Index < nextIndex {
				a.queue.Remove(entry)
				continue
			}
		}

		// If the entry was not in the cache, read it from the log reader.
		a.reader.Reset(nextIndex)
		indexed := a.reader.NextEntry()
		if indexed != nil {
			entriesList.PushBack(indexed.Entry)
			size += indexed.Entry.XXX_Size()
			nextIndex++
			if size >= maxBatchSize {
				break
			}
		} else {
			break
		}
	}

	// Convert the linked list into a slice
	entries := make([]*RaftLogEntry, 0, entriesList.Len())
	entry := entriesList.Front()
	for entry != nil {
		entries = append(entries, entry.Value.(*RaftLogEntry))
		entry = entry.Next()
	}

	// Add the entries to the request builder and return the request.
	request.Entries = entries
	return request
}

func (a *memberAppender) sendAppendRequest(request *AppendRequest) {
	// Start the append to the member.
	startTime := time.Now()

	client, err := a.server.cluster.getClient(a.member.MemberId)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), a.server.electionTimeout)
	defer cancel()

	a.server.logSendTo("AppendRequest", request, a.member.MemberId)
	response, err := client.Append(ctx, request)

	if err == nil {
		a.server.logReceiveFrom("AppendResponse", response, a.member.MemberId)
		if response.Status == ResponseStatus_OK {
			a.handleAppendResponse(request, response, startTime)
		} else {
			a.handleAppendFailure(request, response, startTime)
		}
	} else {
		a.server.logErrorFrom("AppendRequest", err, a.member.MemberId)
		a.handleAppendError(request, err, startTime)
	}
}

func (a *memberAppender) commit(time time.Time) {
	// Send a commit event to the parent appender.
	a.commitCh <- memberCommit{
		member: a,
		index:  a.matchIndex,
		time:   time,
	}
}

func (a *memberAppender) handleAppendResponse(request *AppendRequest, response *AppendResponse, startTime time.Time) {
	// Reset the member failure count to avoid empty heartbeats.
	a.succeed()

	// If replication succeeded then trigger commit futures.
	if response.Succeeded {
		// If the replica returned a valid match index then update the existing match index.
		a.matchIndex = response.LastLogIndex
		a.nextIndex = a.matchIndex + 1

		// If entries were sent to the follower, update the previous entry term to the term of the
		// last entry in the follower's log.
		if len(request.Entries) > 0 {
			a.prevTerm = request.Entries[response.LastLogIndex-request.PrevLogIndex-1].Term
		}

		// Send a commit event to the parent appender.
		a.commit(startTime)

		// Notify the appender that the next index can be appended.
		a.appendCh <- a.nextIndex
	} else {
		// If the request was rejected, use a double checked lock to compare the response term to the
		// server's term. If the term is greater than the local server's term, transition back to follower.
		a.server.readLock()
		if response.Term > a.server.term {
			a.server.readUnlock()
			a.server.writeLock()
			defer a.server.writeUnlock()
			if response.Term > a.server.term {
				// If we've received a greater term, update the term and transition back to follower.
				a.server.setTerm(response.Term)
				a.server.setLeader("")
				go a.server.becomeFollower()
				return
			}
			return
		} else {
			a.server.readUnlock()
		}

		// If the request was rejected, the follower should have provided the correct last index in their log.
		// This helps us converge on the matchIndex faster than by simply decrementing nextIndex one index at a time.
		// Reset the matchIndex and nextIndex according to the response.
		if response.LastLogIndex < a.matchIndex {
			a.matchIndex = response.LastLogIndex
			log.WithField("memberID", a.server.cluster.member).
				Tracef("Reset match index for %s to %d", a.member.MemberId, a.matchIndex)
			a.nextIndex = a.matchIndex + 1
			log.WithField("memberID", a.server.cluster.member).
				Tracef("Reset next index for %s to %d", a.member.MemberId, a.nextIndex)
		}

		// Notify the appender that the next index can be appended.
		a.requeue()
	}
}

func (a *memberAppender) handleAppendFailure(request *AppendRequest, response *AppendResponse, startTime time.Time) {
	a.fail(startTime)
	a.requeue()
}

func (a *memberAppender) handleAppendError(request *AppendRequest, err error, startTime time.Time) {
	a.server.cluster.resetClient(a.member.MemberId)
	a.fail(startTime)
	a.requeue()
}
