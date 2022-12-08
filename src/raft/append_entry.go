package raft

type AppendEntriesArgs struct {
	Term         int
	LeaderId     int
	PrevLogIndex int //前一次是为了验证follower的日志是否是正确的
	PrevLogTerm  int
	Entries      []Entry //日志实体
	LeaderCommit int
}

type AppendEntriesReply struct {
	Term     int
	Success  bool
	Conflict bool
	XTerm    int //这三个x是为了加速回退机制的 性能优化  conflictTerm = log[prevLogIndex].Term 并且查找其log中term等于 conflictTerm的第一个log的下标 (Index)。
	XIndex   int
	XLen     int
}

//发送心跳包的rpc
func (rf *Raft) appendEntries(heartbeat bool) {
	lastLog := rf.log.lastLog()
	for peer, _ := range rf.peers {
		if peer == rf.me {
			rf.resetElectionTimer()
			continue
		}
		// rules for leader 3
		if lastLog.Index >= rf.nextIndex[peer] || heartbeat {
			nextIndex := rf.nextIndex[peer]
			if nextIndex <= 0 {
				nextIndex = 1
			}
			if lastLog.Index+1 < nextIndex {
				nextIndex = lastLog.Index
			}

			//这里是默认的prelog 为nextIndex-1  如果发现不匹配的话 还会发生回退
			prevLog := rf.log.at(nextIndex - 1)
			args := AppendEntriesArgs{
				Term:         rf.currentTerm,
				LeaderId:     rf.me,
				PrevLogIndex: prevLog.Index,
				PrevLogTerm:  prevLog.Term,
				Entries:      make([]Entry, lastLog.Index-nextIndex+1),
				LeaderCommit: rf.commitIndex,
			}
			copy(args.Entries, rf.log.slice(nextIndex))
			go rf.leaderSendEntries(peer, &args)
		}
	}
}

func (rf *Raft) leaderSendEntries(serverId int, args *AppendEntriesArgs) {
	var reply AppendEntriesReply
	ok := rf.sendAppendEntries(serverId, args, &reply)
	if !ok {
		return
	}
	rf.mu.Lock()
	defer rf.mu.Unlock()
	if reply.Term > rf.currentTerm {
		rf.setNewTerm(reply.Term)
		return
	}
	if args.Term == rf.currentTerm {
		// rules for leader 3.1
		if reply.Success { //正常的日志同步逻辑
			match := args.PrevLogIndex + len(args.Entries)
			next := match + 1
			rf.nextIndex[serverId] = max(rf.nextIndex[serverId], next)
			rf.matchIndex[serverId] = max(rf.matchIndex[serverId], match)
			DPrintf("[%v]: %v append success next %v match %v", rf.me, serverId, rf.nextIndex[serverId], rf.matchIndex[serverId])
		} else if reply.Conflict { //这里就是如果处理发生了冲突的情况
			/*
				先通过任期来试图找到发生冲突时候的日志 没有任期冲突的日志就直接返回冲突日志的下标
						                           这时有两种情况:
					1.Leader接收到冲突响应后，其应该首先查找conflictTerm的log。如果它在entry中发现为conflictTerm的log，
					其应该设置 nextIndex为其log中term等于confiictTerm的最后一个entry的下标(Index)。
					2.如果Leader在当前term中没有发现entry，其应该设置nextIndex = conflictIndex。
			*/
			DPrintf("[%v]: Conflict from %v %#v", rf.me, serverId, reply)
			if reply.XTerm == -1 { //没有prelog日志的情况  直接就把follower日志的长度作为下依次心跳日志同步的下标
				rf.nextIndex[serverId] = reply.XLen
			} else { //有prelog日志 但是没有匹配的情况
				lastLogInXTerm := rf.findLastLogInTerm(reply.XTerm)
				DPrintf("[%v]: lastLogInXTerm %v", rf.me, lastLogInXTerm)
				if lastLogInXTerm > 0 { //在leader节点找到了 冲突的任期
					rf.nextIndex[serverId] = lastLogInXTerm
				} else { //如果没有冲突的任期 就直接把下一个变成 follower 冲突日志的下标
					rf.nextIndex[serverId] = reply.XIndex
				}
			}

			DPrintf("[%v]: leader nextIndex[%v] %v", rf.me, serverId, rf.nextIndex[serverId])
		} else if rf.nextIndex[serverId] > 1 {
			rf.nextIndex[serverId]--
		}
		rf.leaderCommitRule()
	}
}

func (rf *Raft) findLastLogInTerm(x int) int {
	for i := rf.log.lastLog().Index; i > 0; i-- {
		term := rf.log.at(i).Term
		if term == x {
			return i
		} else if term < x {
			break
		}
	}
	return -1
}

//日志提交也就是持久化
func (rf *Raft) leaderCommitRule() {
	// leader rule 4
	if rf.state != Leader {
		return
	}

	for n := rf.commitIndex + 1; n <= rf.log.lastLog().Index; n++ {
		if rf.log.at(n).Term != rf.currentTerm {
			continue
		}
		counter := 1
		for serverId := 0; serverId < len(rf.peers); serverId++ {
			if serverId != rf.me && rf.matchIndex[serverId] >= n {
				counter++
			}
			if counter > len(rf.peers)/2 {
				rf.commitIndex = n
				DPrintf("[%v] leader尝试提交 index %v", rf.me, rf.commitIndex)
				rf.apply()
				break
			}
		}
	}
}

// AppendEntries 接受心跳日志的包的rpc
func (rf *Raft) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	DPrintf("[%d]: (term %d) follower 收到 [%v] AppendEntries %v, prevIndex %v, prevTerm %v", rf.me, rf.currentTerm, args.LeaderId, args.Entries, args.PrevLogIndex, args.PrevLogTerm)
	// rules for servers
	// all servers 2
	reply.Success = false
	reply.Term = rf.currentTerm
	if args.Term > rf.currentTerm {
		rf.setNewTerm(args.Term)
		return
	}

	// append entries rpc 1
	if args.Term < rf.currentTerm {
		return
	}
	rf.resetElectionTimer()

	// candidate rule 3
	if rf.state == Candidate {
		rf.state = Follower
	}
	// append entries rpc 2
	if rf.log.lastLog().Index < args.PrevLogIndex {
		reply.Conflict = true
		reply.XTerm = -1
		reply.XIndex = -1
		reply.XLen = rf.log.len()
		DPrintf("[%v]: Conflict XTerm %v, XIndex %v, XLen %v", rf.me, reply.XTerm, reply.XIndex, reply.XLen)
		return
	}
	if rf.log.at(args.PrevLogIndex).Term != args.PrevLogTerm {
		reply.Conflict = true
		xTerm := rf.log.at(args.PrevLogIndex).Term
		for xIndex := args.PrevLogIndex; xIndex > 0; xIndex-- {
			if rf.log.at(xIndex-1).Term != xTerm {
				reply.XIndex = xIndex
				break
			}
		}
		reply.XTerm = xTerm
		reply.XLen = rf.log.len()
		DPrintf("[%v]: Conflict XTerm %v, XIndex %v, XLen %v", rf.me, reply.XTerm, reply.XIndex, reply.XLen)
		return
	}

	for idx, entry := range args.Entries {
		// append entries rpc 3
		if entry.Index <= rf.log.lastLog().Index && rf.log.at(entry.Index).Term != entry.Term {
			rf.log.truncate(entry.Index)
			rf.persist()
		}
		// append entries rpc 4
		if entry.Index > rf.log.lastLog().Index {
			rf.log.append(args.Entries[idx:]...)
			DPrintf("[%d]: follower append [%v]", rf.me, args.Entries[idx:])
			rf.persist()
			break
		}
	}

	// append entries rpc 5
	if args.LeaderCommit > rf.commitIndex {
		rf.commitIndex = min(args.LeaderCommit, rf.log.lastLog().Index)
		rf.apply()
	}
	reply.Success = true
}

func (rf *Raft) sendAppendEntries(server int, args *AppendEntriesArgs, reply *AppendEntriesReply) bool {
	ok := rf.peers[server].Call("Raft.AppendEntries", args, reply)
	return ok
}
