package raft

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"time"
)

type workMode int

func (m workMode) String() string {
	return [3]string{"follower", "candidate", "leader"}[m]
}

const (
	followerMode workMode = iota
	candidateMode
	leaderMode
)

const (
	electionTimeout                = time.Millisecond * 3000
	maxStartElectionDelay          = electionTimeout / 2
	leaderAppendEntriesRPCInterval = time.Millisecond * 50
)

type Raft struct {
	ins         RaftInstance
	ctx         context.Context
	instanceNum int
	mode        workMode

	// raft states
	currentTerm Term
	votedFor    int
	log         []*Log
	// index of highest log entry known to be committed (initialized to 0, increases monotonically)
	commitIndex LogIndex
	// index of highest log entry applied to state machine (initialized to 0, increases monotonically)
	lastApplied LogIndex

	// all state fields
	resetElectionTimeoutTime time.Time

	// follower mode fields

	// candidate mode fields
	startElectionTime time.Time
	electionStarted   bool
	voteGrantedCount  int

	// leader mode fields
	lastAppendEntriesRPCTime time.Time
	// for each server, index of the next log entry to send to that server (initialized to leader last log index + 1)
	nextIndex []LogIndex
	// for each server, index of highest log entry known to be replicated on server (initialized to 0, increases monotonically)
	matchIndex []LogIndex
}

func NewRaft(ctx context.Context, instanceNum int, ins RaftInstance) *Raft {
	if instanceNum <= 0 {
		log.Fatalf("instanceNum should be larger than 0")
	}

	if ins.ID() >= instanceNum {
		log.Fatalf("instance ID should be smaller than %d", instanceNum)
	}

	raft := &Raft{
		ins:         ins,
		ctx:         ctx,
		instanceNum: instanceNum,
		mode:        followerMode,
		resetElectionTimeoutTime: time.Now(),
		// init raft states
		currentTerm: 0,
		votedFor:    -1,
		log:         nil,
		commitIndex: 0,
		lastApplied: 0,
	}

	go raft.routine()

	return raft
}

func (r *Raft) ID() int {
	return r.ins.ID()
}

func (r *Raft) String() string {
	return fmt.Sprintf("Raft<%d>", r.ins.ID())
}

func (r *Raft) routine() {
	ticker := time.NewTicker(time.Millisecond * 10)
	defer ticker.Stop()
	log.Printf("%s start running ...", r)

forloop:
	for {
		select {
		case <-ticker.C:
			switch r.mode {
			case leaderMode:
				r.leaderTick()
			case candidateMode:
				r.candidateTick()
			case followerMode:
				r.followerTick()
			default:
				log.Fatalf("invalid mode: %d", r.mode)
			}

		case msg := <-r.ins.Recv():
			//log.Printf("%s received msg: %+v", r, msg)
			r.handleMsg(msg)
		case inputLog := <-r.ins.InputLog():
			r.handleInputLog(inputLog)
		case <-r.ctx.Done():
			log.Printf("%s stopped.", r)
			break forloop
		}
	}
}

func (r *Raft) handleMsg(_msg RPCMessage) {
	//All Servers:
	//?If RPC request or response contains Term T > currentTerm:
	//set currentTerm = T, convert to follower (?.1)

	if r.currentTerm < _msg.Term() {
		r.enterFollowerMode(_msg.Term())
	}

	switch msg := _msg.(type) {
	case *AppendEntriesACKMessage:
		//log.Printf("AppendEntries ACK: %+v", msg)
	case *AppendEntriesMessage:
		r.handleAppendEntries(msg)
	case *RequestVoteMessage:
		r.handleRequestVote(msg)
	case *RequestVoteACKMessage:
		r.handleRequestVoteACKMessage(msg)
	default:
		log.Fatalf("unexpected message type: %T", _msg)
	}
}

func (r *Raft) handleRequestVote(msg *RequestVoteMessage) {
	grantVote := r._handleRequestVote(msg)
	log.Printf("%s grant vote %+v: %v", r, msg, grantVote)
	// send grant vote ACK
	// Results:
	//Term currentTerm, for candidate to update itself
	//voteGranted true means candidate received vote
	ackMsg := &RequestVoteACKMessage{
		term:        r.currentTerm,
		voteGranted: grantVote,
	}
	r.ins.Send(msg.candidateId, ackMsg)
}

func (r *Raft) _handleRequestVote(msg *RequestVoteMessage) bool {
	//1. Reply false if Term < currentTerm (§5.1)
	//2. If votedFor is null or candidateId, and candidate’s log is at least as up-to-date as receiver’s log, grant vote (§5.2, §5.4)

	if msg.term < r.currentTerm {
		return false
	}

	grantVote := (r.votedFor == -1 || r.votedFor == msg.candidateId) && r.isLogUpToDate(msg.lastLogTerm, msg.lastLogIndex)
	if grantVote {
		r.votedFor = msg.candidateId
	}
	return grantVote
}

func (r *Raft) handleRequestVoteACKMessage(msg *RequestVoteACKMessage) {
	if r.mode != candidateMode {
		// if not in candidate mode anymore, just ignore this packet
		return
	}

	r.voteGrantedCount += 1
	if r.voteGrantedCount >= (r.instanceNum/2)+1 {
		// become leader
		r.enterLeaderMode()
	}
}

func (r *Raft) handleAppendEntries(msg *AppendEntriesMessage) {
	r.resetElectionTimeout()
	success := r.handleAppendEntriesImpl(msg)
	r.ins.Send(msg.leaderId, &AppendEntriesACKMessage{
		term:    r.currentTerm,
		success: success,
	})

}

func (r *Raft) handleAppendEntriesImpl(msg *AppendEntriesMessage) bool {
	//1. Reply false if term < currentTerm (§5.1)
	if msg.term < r.currentTerm {
		return false
	}

	//2. Reply false if log doesn’t contain an entry at prevLogIndex whose term matches prevLogTerm (§5.3)
	containsPrevLog, prevIdx := r.containsLog(msg.prevLogTerm, msg.prevLogIndex)
	if !containsPrevLog {
		return false
	}

	replaceIdxStart := prevIdx + 1

	for i, entry := range msg.entries {
		if replaceIdxStart+i < len(r.log) {
			//3. If an existing entry conflicts with a new one (same index but different terms), delete the existing entry and all that follow it (§5.3)
			replaceLog := r.log[replaceIdxStart+i]
			if replaceLog.index != entry.index {
				log.Fatalf("Log Index Mismatch: %d & %d", replaceLog.index, entry.index)
			}
			if replaceLog.term == entry.term {
				// normal case
			} else {
				replaceLog.term, replaceLog.data = entry.term, entry.data
				r.log = r.log[0:replaceIdxStart + i + 1]
			}
		} else {
			//4. Append any new entries not already in the log
			r.log = append(r.log, entry)
		}
	}

	log.Printf("%s LOG %d.%d", r, r.lastLogTerm(), r.lastLogIndex())
	//5. If leaderCommit > commitIndex, set commitIndex = min(leaderCommit, index of last new entry)
	if len(msg.entries) > 0 {
		if msg.leaderCommit > r.commitIndex {
			commitIndex := msg.leaderCommit
			if commitIndex > msg.entries[len(msg.entries)-1].index {
				commitIndex = msg.entries[len(msg.entries)-1].index
			}
			if commitIndex < r.commitIndex {
				log.Fatalf("New Commit Index Is %d, But Current Commit Index Is %d", commitIndex, r.commitIndex)
			}
			r.commitIndex = commitIndex
		}
	}

	return true
}

// isLogUpToDate determines if the log of specified Term and index is at least as up-to-date as r.log
func (r *Raft) isLogUpToDate(term Term, logIndex LogIndex) bool {
	myterm := r.lastLogTerm()
	if term < myterm {
		return false
	} else if term > myterm {
		return true
	}

	return logIndex >= r.lastLogIndex()
}

func (r *Raft) followerTick() {
	now := time.Now()
	if now.Sub(r.resetElectionTimeoutTime) > electionTimeout {
		r.enterCandidateMode()
	}
}

func (r *Raft) candidateTick() {
	now := time.Now()

	if now.Sub(r.resetElectionTimeoutTime) > electionTimeout {
		log.Printf("%s: election timeout in candidate mode, restarting election ...", r)
		r.prepareElection()
		return
	}

	if !r.electionStarted {
		if now.After(r.startElectionTime) {
			r.startElection()
		}
	}

}

func (r *Raft) leaderTick() {
	now := time.Now()
	if now.Sub(r.lastAppendEntriesRPCTime) >= leaderAppendEntriesRPCInterval {
		// time to broadcast AppendEntriesRPC
		//log.Printf("%s: Broadcast AppendEntries ...", r)
		r.lastAppendEntriesRPCTime = now
		r.broadcastAppendEntries()
	}
}

func (r *Raft) resetElectionTimeout() {
	r.resetElectionTimeoutTime = time.Now()
}

func (r *Raft) prepareElection() {
	r.assureInMode(candidateMode)

	log.Printf("%s prepare election ...", r)
	r.startElectionTime = time.Now().Add(time.Duration(rand.Int63n(int64(maxStartElectionDelay))))
	r.electionStarted = false
	r.resetElectionTimeout()
	log.Printf("%s set start election time = %s", r, r.startElectionTime)
}

func (r *Raft) startElection() {
	r.assureInMode(candidateMode)

	if r.electionStarted {
		log.Panicf("election already started")
	}
	//On conversion to candidate, start election:
	//?Increment currentTerm
	//?Vote for self
	//?Reset election timer
	//?Send RequestVote RPCs to all other servers
	log.Printf("%s start election ...", r)
	r.newTerm(r.currentTerm + 1)
	r.electionStarted = true
	r.votedFor = r.ID()    // vote for self
	r.voteGrantedCount = 1 // vote for self in the beginning of election
	r.sendRequestVote()
}

func (r *Raft) sendRequestVote() {
	//Arguments:
	//Term candidate’s Term
	//candidateId candidate requesting vote
	//lastLogIndex index of candidate’s last log entry (§5.4)
	//lastLogTerm Term of candidate’s last log entry (§5.4)
	msg := &RequestVoteMessage{
		term:         r.currentTerm,
		candidateId:  r.ID(),
		lastLogIndex: r.lastLogIndex(),
		lastLogTerm:  r.lastLogTerm(),
	}
	r.ins.Broadcast(msg)
}

func (r *Raft) assureInMode(mode workMode) {
	if r.mode != mode {
		log.Fatalf("%s should in %s mode, but in %s mode", r, mode, r.mode)
	}
}

// enter follower mode with new Term
func (r *Raft) enterFollowerMode(term Term) {
	log.Printf("%s change mode: %s ==> %s, new Term = %d", r, r.mode, followerMode, term)
	r.newTerm(term)
	r.mode = followerMode
}

func (r *Raft) newTerm(term Term) {
	if r.currentTerm >= term {
		log.Fatalf("current Term is %d, can not enter follower mode with Term %d", r.currentTerm, term)
	}
	r.currentTerm = term
	r.votedFor = -1
}

func (r *Raft) enterCandidateMode() {
	if r.mode != followerMode {
		log.Fatalf("only follower can convert to candidate, but current mode is %s", r.mode)
	}

	log.Printf("%s change mode: %s ==> %s", r, r.mode, candidateMode)
	r.mode = candidateMode
	r.prepareElection()
}

func (r *Raft) enterLeaderMode() {
	if r.mode != candidateMode {
		log.Fatalf("only candidate can convert to leader, but current mode is %s", r.mode)
	}

	log.Printf("%s change mode: %s ==> %s", r, r.mode, leaderMode)
	r.mode = leaderMode
	log.Printf("NEW LEADER ELECTED: %d !!!", r.ID())
	r.lastAppendEntriesRPCTime = time.Time{}
	r.nextIndex = make([]LogIndex, r.instanceNum)
	for i := range r.nextIndex {
		r.nextIndex[i] = r.lastLogIndex() + 1
	}
	r.matchIndex = make([]LogIndex, r.instanceNum)
}

func (r *Raft) lastLogIndex() LogIndex {
	if len(r.log) > 0 {
		return r.log[len(r.log)-1].index
	} else {
		return 0
	}
}

func (r *Raft) lastLogTerm() Term {
	if len(r.log) > 0 {
		return r.log[len(r.log)-1].term
	} else {
		return 0
	}
}
func (r *Raft) broadcastAppendEntries() {
	for insID := 0; insID < r.instanceNum; insID++ {
		if insID == r.ID() {
			continue
		}

		logIndex := r.nextIndex[insID] // next index for the instance to receive
		var entries []*Log
		nextlogidx := r.locateLog(logIndex)
		var prevLogTerm Term
		var prevLogIndex LogIndex
		if nextlogidx > 0 {
			prevLogTerm = r.log[nextlogidx-1].term
			prevLogIndex = r.log[nextlogidx-1].index
		}

		for i:= nextlogidx;i<len(r.log);i++ {
			entries = append(entries, r.log[i])
		}

		//log.Printf("%s APPEND LOG %d ~ %d", r, nextlogidx, len(r.log)-1)

		msg := &AppendEntriesMessage{
			term:         r.currentTerm,
			leaderId:     r.ID(),
			prevLogTerm:  prevLogTerm,
			prevLogIndex: prevLogIndex,
			leaderCommit: r.commitIndex,
			entries: entries,
		}

		r.ins.Send(insID, msg)
	}
}

func (r *Raft) handleInputLog(data LogData) {
	if r.mode != leaderMode {
		//log.Printf("Not Leader, Data Ignored: %s", data)
		return
	}

	newlog := &Log{
		term:  r.currentTerm,
		index: r.lastLogIndex() + 1,
		data:  data,
	}

	r.log = append(r.log, newlog)
	log.Printf("%s LOG %d.%d", r, r.lastLogTerm(), r.lastLogIndex())
}

func (r *Raft) locateLog(index LogIndex) int {
	r.validateLog()
	return int(index)-1
}

func (r *Raft) containsLog(term Term, index LogIndex) (bool, int) {
	idx := r.locateLog(index)
	if idx == -1 {
		return true, -1
	} else if idx >= len(r.log) {
		return false, idx
	}

	_log := r.log[idx]
	if _log.index != index {
		log.Fatalf("Should Equal")
	}
	return _log.term == term, idx
}

func (r *Raft) validateLog() {
	prevLogIndex := LogIndex(0)
	for _, _log :=range r.log {
		if _log.index != prevLogIndex + 1 {
			log.Fatalf("Invalid Log Index: %d, Should be %d", _log.index, prevLogIndex+1)
		}
		prevLogIndex = _log.index
	}
}