package kvraft

import (
	"bytes"
	"encoding/gob"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"6.5840/labgob"
	"6.5840/labrpc"
	"6.5840/raft"
)

type KVServer struct {
	mu      sync.Mutex
	me      int
	rf      *raft.Raft
	applyCh chan raft.ApplyMsg
	dead    int32 // set by Kill()

	maxraftstate int // snapshot if log grows this big

	persister *raft.Persister
	// Your definitions here.

	//duplicateMap: use to handle mulity RPC request
	// duplicateMap map[int64]duplicateType

	lastAppliedIndex int //最近添加到状态机中的raft层的log的index
	//lastInclude
	lastIncludeIndex int
	//log state machine
	kvMap map[string]string

	//缓存的log, seq->index,reply
	duplicateMap map[int64]duplicateType
}

type duplicateType struct {
	Offset int32
	Reply  string
}

func (kv *KVServer) Get(args *GetArgs, reply *GetReply) {
	// Your code here.
	reply.Value = "i should not with ok symble"
	reply.Err = ErrWrongLeader
	reply.LeaderId = kv.rf.GetleaderId()
	reply.ServerId = kv.me

	//判断自己是不是leader
	if _, ok := kv.rf.GetState(); ok {
		// DPrintf("server [%d] [info] i am leader", kv.me)
	} else {
		// DPrintf("server [%d] [info] i am not leader ,leader is [%d]", kv.me, reply.LeaderId)
		return
	}

	//判断自己有没有从重启中恢复完毕状态机
	if !kv.rf.IisBack {
		DPrintf("server [%d] [recovering] reject a [Get]🔰 args[%v]", kv.me, *args)
		reply.Err = ErrWaitForRecover
		kv.rf.Start(Op{
			OpType: emptyT,
		})
		return
	}

	readLastIndex := kv.rf.GetCommitIndex()
	term := kv.rf.GetTerm()
	//需要发送一轮心跳获得大多数回复，只是为了确定没有一个任期更加新的leader，保证自己的数据不是脏的
	if kv.rf.CheckIfDepose() {
		reply.Err = ErrWrongLeader
		return
	}
	//return false ,但是进入下面代码段的时候，发现自己又不是leader了，捏麻麻的
	kv.mu.Lock()
	defer kv.mu.Unlock()
	//跟raft层之间的同步问题，raft刚当选leader的时候，还没有

	//直接返回
	value, find := kv.kvMap[args.Key]
	if find {
		if kv.lastAppliedIndex >= readLastIndex && kv.rf.GetLeader() && term == kv.rf.GetTerm() {
			reply.Err = OK
			reply.Value = value
			DPrintf("server [%d] [Get] [ok] lastAppliedIndex[%d] readLastIndex[%d]", kv.me, kv.lastAppliedIndex, readLastIndex)
			DPrintf("server [%d] [Get] [Ok] the get args[%v] reply[%v]", kv.me, *args, *reply)
		} else {
			reply.Err = ErrWaitForRecover
			// DPrintf("server [%d] [Get] [ErrWaitForRecover] kv.lastAppliedIndex < readLastIndex args[%v] reply[%v]", kv.me, *args, *reply)
		}

	} else {
		reply.Err = ErrNoKey
		DPrintf("server [%d] [Get] [NoKey] the get args[%v] reply[%v]", kv.me, *args, *reply)
		DPrintf("server [%d] [map] -> %v", kv.me, kv.kvMap)
	}

}

func (kv *KVServer) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {

	// Your code here.
	reply.LeaderId = kv.rf.GetleaderId()
	reply.Err = ErrWrongLeader
	reply.ServerId = kv.me
	if _, ok := kv.rf.GetState(); ok {
		// DPrintf("server [%d] [info] i am leader", kv.me)
	} else {
		// DPrintf("server [%d] [info] i am not leader ,leader is [%d]", kv.me, reply.LeaderId)
		return
	}

	op := Op{
		ClientId: args.ClientId,
		Offset:   args.LatestOffset,
		Key:      args.Key,
		Value:    args.Value,
	}

	switch args.Op {
	case "Put":
		op.OpType = putT
	case "Append":
		op.OpType = appendT
	default:
		log.Fatalf("unreconize put append args.Op:%s", args.Op)
	}

	// DPrintf("server [%d] [PutAppend] 📨receive a args[%v]", kv.me, *args)
	// defer DPrintf("server [%d] [PutAppend] 📨complete a args[%v]", kv.me, *args)
	//start前需要查看本地log缓存是否有seq

	//这里通过缓存提交，一方面提高了kvserver应对网络错误的回复速度，另一方面进行了第一层的重复检测
	//但是注意可能同时有两个相同的getDuplicateMap通过这里

	kv.mu.Lock()
	if args.LatestOffset < kv.duplicateMap[args.ClientId].Offset {
		kv.mu.Unlock()
		return
	}
	if args.LatestOffset == kv.duplicateMap[args.ClientId].Offset {
		reply.Err = OK
		kv.mu.Unlock()
		return
	}
	kv.mu.Unlock()

	//没有在本地缓存发现过seq
	//向raft提交操作
	index, term, isleader := kv.rf.Start(op)

	if !isleader {
		return
	}
	kv.rf.SendAppendEntriesToAll()
	DPrintf("server [%d] submit to raft key[%v] value[%v]", kv.me, op.Key, op.Value)
	//提交后阻塞等待
	//等待applyCh拿到对应的index，比对seq是否正确
	startWait := time.Now()
	for !kv.killed() {
		time.Sleep(1 * time.Millisecond)
		kv.mu.Lock()

		if index <= kv.lastAppliedIndex {
			//双重防重复
			if args.LatestOffset < kv.duplicateMap[args.ClientId].Offset {
				kv.mu.Unlock()
				return
			}
			if args.LatestOffset == kv.duplicateMap[args.ClientId].Offset {
				reply.Err = OK
				kv.mu.Unlock()
				return
			}

			// DPrintf("server [%d] [PutAppend] appliedIndex available :PutAppend index[%d] lastAppliedIndex[%d]", kv.me, index, kv.lastAppliedIndex)
			if term != kv.rf.GetTerm() {
				//term不匹配了，说明本次提交失效
				kv.mu.Unlock()
				return
			} //term匹配，说明本次提交一定是有效的

			reply.Err = OK
			// DPrintf("server [%d] [PutAppend] success args.index[%d], args[%v] reply[%v]", kv.me, index, *args, *reply)
			kv.mu.Unlock()
			if _, isleader := kv.rf.GetState(); !isleader {
				reply.Err = ErrWrongLeader
			}
			return
		}
		kv.mu.Unlock()
		//超过2s没等到applych，那就返回wrong
		if time.Since(startWait).Milliseconds() > 500 {
			DPrintf("server [%d] [PutAppend] fail [time out] args.index[%d]", kv.me, index)
			return
		}
	}

}

// state machine
// 将value重新转换为 Op，添加到本地kvMap中
func (kv *KVServer) HandleApplych() {
	for !kv.killed() {
		select {
		case raft_type := <-kv.applyCh:
			if kv.killed() {
				return
			}
			kv.mu.Lock()
			if raft_type.CommandValid {
				kv.HandleApplychCommand(raft_type)
				kv.checkifNeedSnapshot(raft_type.CommandIndex)
				kv.lastAppliedIndex = raft_type.CommandIndex
			} else if raft_type.SnapshotValid {
				DPrintf("📷 server [%d] receive raftSnapshotIndex[%d]", kv.me, raft_type.SnapshotIndex)
				kv.HandleApplychSnapshot(raft_type)
			} else {
				log.Fatalf("Unrecordnized applyArgs type")
			}
			kv.mu.Unlock()
		}

	}
}

func (kv *KVServer) HandleApplychCommand(raft_type raft.ApplyMsg) {
	op_type, ok := raft_type.Command.(Op)
	if !ok {
		log.Fatalf("raft applyArgs.command -> Op 失败,raft_type.Command = %v", raft_type.Command)
	}

	if op_type.OpType == emptyT {
		return
	}

	switch op_type.OpType {
	case putT:
		//更新状态机
		//有可能有多个start重复执行，所以这一步要检验重复
		if op_type.Offset <= kv.duplicateMap[op_type.ClientId].Offset {
			DPrintf("⛔server [%d] [Put] [%v] lastapplied[%v]find in the cache and discard %v", kv.me, op_type, kv.lastAppliedIndex, kv.kvMap)
			return
		}
		kv.duplicateMap[op_type.ClientId] = duplicateType{
			Offset: op_type.Offset,
			Reply:  "",
		}
		kv.kvMap[op_type.Key] = op_type.Value
		// DPrintf("server [%d] [Update] [Put]->[%s,%s] [map] -> %v", kv.me, op_type.Key, op_type.Value, kv.kvMap)
		DPrintf("server [%d] [Update] [Put]->[%s : %s] ", kv.me, op_type.Key, op_type.Value)
	case appendT:
		//更新状态机
		if op_type.Offset <= kv.duplicateMap[op_type.ClientId].Offset {
			DPrintf("⛔server [%d] [Append] [%v] lastapplied[%v]find in the cache and discard %v", kv.me, op_type, kv.lastAppliedIndex, kv.kvMap)
			return
		}
		kv.duplicateMap[op_type.ClientId] = duplicateType{
			Offset: op_type.Offset,
			Reply:  "",
		}
		kv.kvMap[op_type.Key] += op_type.Value
		DPrintf("server [%d] [Update] [Append]->[%s : %s]", kv.me, op_type.Key, op_type.Value)
	case getT:
		log.Fatalf("日志中不应该出现getType")
	default:
		log.Fatalf("日志中出现未知optype = [%d]", op_type.OpType)
	}

}

// 被动快照,follower接受从leader传来的snapshot
func (kv *KVServer) HandleApplychSnapshot(raft_type raft.ApplyMsg) {
	if raft_type.SnapshotIndex < kv.lastAppliedIndex {
		return
	}
	snapshot := raft_type.Snapshot
	kv.readPersist(snapshot)
	DPrintf("server [%d] passive📷 lastAppliedIndex[%d] -> [%d]", kv.me, kv.lastAppliedIndex, raft_type.SnapshotIndex)
	kv.lastAppliedIndex = raft_type.SnapshotIndex

}

// 主动快照,每一个服务器都在自己log超标的时候启动快照
func (kv *KVServer) checkifNeedSnapshot(spanshotindex int) {
	if kv.maxraftstate == -1 {
		return
	}
	if !kv.rf.IfNeedExceedLog(kv.maxraftstate) {
		return
	} //需要进行快照了

	DPrintf("server [%d] need snapshot limit[%d] curRaftStatesize[%d] snapshotIndex[%d]", kv.me, kv.maxraftstate, kv.persister.RaftStateSize(), spanshotindex)
	//首先查看一下自己的状态机应用到了那一步

	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(kv.duplicateMap); err != nil {
		log.Fatalf("snapshot duplicateMap encoder fail:%s", err)
	}
	if err := enc.Encode(kv.kvMap); err != nil {
		log.Fatalf("snapshot kvMap encoder fail:%s", err)
	}

	//将状态机传了进去
	kv.rf.Snapshot(spanshotindex, buf.Bytes())

}

// 被动快照
func (kv *KVServer) readPersist(data []byte) {
	if data == nil || len(data) < 1 {
		return
	}
	DPrintf("server [%d] passive 📷 len of snapshotdate[%d] ", kv.me, len(data))
	DPrintf("server [%d] before map[%v]", kv.me, kv.kvMap)
	r := bytes.NewBuffer(data)
	d := gob.NewDecoder(r)

	kvMap := make(map[string]string)
	duplicateMap := make(map[int64]duplicateType)
	if err := d.Decode(&duplicateMap); err != nil {
		log.Fatalf("decode err:%s", err)
	}
	if err := d.Decode(&kvMap); err != nil {
		log.Fatalf("decode err:%s", err)
	}
	kv.kvMap = kvMap
	kv.duplicateMap = duplicateMap

	DPrintf("server [%d] after map[%v]", kv.me, kv.kvMap)

}

// the tester calls Kill() when a KVServer instance won't
// be needed again. for your convenience, we supply
// code to set rf.dead (without needing a lock),
// and a killed() method to test rf.dead in
// long-running loops. you can also add your own
// code to Kill(). you're not required to do anything
// about this, but it may be convenient (for example)
// to suppress debug output from a Kill()ed instance.
func (kv *KVServer) Kill() {
	atomic.StoreInt32(&kv.dead, 1)
	kv.rf.Kill()
	// Your code here, if desired.
}

func (kv *KVServer) killed() bool {
	z := atomic.LoadInt32(&kv.dead)
	return z == 1
}

// servers[] contains the ports of the set of
// servers that will cooperate via Raft to
// form the fault-tolerant key/value service.
// me is the index of the current server in servers[].
// the k/v server should store snapshots through the underlying Raft
// implementation, which should call persister.SaveStateAndSnapshot() to
// atomically save the Raft state along with the snapshot.
// the k/v server should snapshot when Raft's saved state exceeds maxraftstate bytes,
// in order to allow Raft to garbage-collect its log. if maxraftstate is -1,
// you don't need to snapshot.
// StartKVServer() must return quickly, so it should start goroutines
// for any long-running work.
func StartKVServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int) *KVServer {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Op{})
	labgob.Register(map[string]string{})
	labgob.Register(map[int64]duplicateType{})
	kv := new(KVServer)
	kv.me = me
	kv.maxraftstate = maxraftstate
	kv.persister = persister
	// You may need initialization code here.

	kv.applyCh = make(chan raft.ApplyMsg)
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)

	// You may need initialization code here.
	kv.lastAppliedIndex = 0
	kv.lastIncludeIndex = 0
	//state machine
	kv.kvMap = make(map[string]string)
	//log
	kv.duplicateMap = make(map[int64]duplicateType)

	kv.readPersist(persister.ReadSnapshot())
	go kv.HandleApplych()
	// go kv.HandleSnapshot()
	// go kv.handleGetTask()
	DPrintf("server [%d] restart", kv.me)
	return kv
}
