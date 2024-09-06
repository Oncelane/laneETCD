package kvraft

import (
	"crypto/rand"
	"log"
	"math/big"
	"time"

	"6.5840/labrpc"
)

type Clerk struct {
	servers []*labrpc.ClientEnd
	// You will have to modify this struct.
	nextSendLocalId int
	LatestOffset    int32
	clientId        int64
	cTos            []int
	sToc            []int
}

func nrand() int64 {
	max := big.NewInt(int64(1) << 62)
	bigx, _ := rand.Int(rand.Reader, max)
	x := bigx.Int64()
	return x
}

func MakeClerk(servers []*labrpc.ClientEnd) *Clerk {
	ck := new(Clerk)
	ck.servers = servers
	// You'll have to add code here.
	ck.nextSendLocalId = int(nrand() % int64(len(servers)))
	ck.LatestOffset = 1
	ck.clientId = nrand()
	ck.cTos = make([]int, len(servers))
	ck.sToc = make([]int, len(servers))
	for i := range ck.cTos {
		ck.cTos[i] = -1
		ck.sToc[i] = -1
	}

	return ck
}

// fetch the current value for a key.
// returns "" if the key does not exist.
// keeps trying forever in the face of all other errors.
//
// you can send an RPC with code like this:
// ok := ck.servers[i].Call("KVServer.Get", &args, &reply)
//
// the types of args and reply (including whether they are pointers)
// must match the declared types of the RPC handler function's
// arguments. and reply must be passed as a pointer.
func (ck *Clerk) Get(key string) string {
	// You will have to modify this function.

	args := GetArgs{
		Key:          key,
		ClientId:     ck.clientId,
		LatestOffset: ck.LatestOffset,
	}

	count := 0
	lastSendLocalId := -1

	for {

		if ck.nextSendLocalId == lastSendLocalId {
			count++
			if count > 5 {
				count = 0
				ck.changeNextSendId()
			}
		}

		reply := GetReply{}
		// DPrintf("clinet [%d] [Get]:send[%d] args[%v]", ck.clientId, ck.nextSendLocalId, args)
		ok := ck.servers[ck.nextSendLocalId].Call("KVServer.Get", &args, &reply)

		//根据reply初始化一下本地server表

		lastSendLocalId = ck.nextSendLocalId
		if !ok {
			// DPrintf("clinet [%d] [Get]:[lost] args[%v]", ck.clientId, args)
			//对面失联，那就换下一个继续发
			ck.changeNextSendId()
			continue
		}

		ck.sToc[reply.ServerId] = ck.nextSendLocalId

		switch reply.Err {
		case OK:
			ck.LatestOffset++
			// DPrintf("clinet [%d] [Get]:[OK] get args[%v] reply[%v]", ck.clientId, args, reply)
			return reply.Value
		case ErrNoKey:
			// DPrintf("clinet [%d] [Get]:[ErrNo key] get args[%v]", ck.clientId, args)
			return ""
		case ErrWrongLeader:
			// DPrintf("clinet [%d] [Get]:[ErrWrong LeaderId][%d] get args[%v] reply[%v]", ck.clientId, ck.nextSendLocalId, args, reply)
			//对方也不知道leader
			if reply.LeaderId == -1 {
				//寻找下一个
				ck.changeNextSendId()
			} else {
				//记录对方返回的不可靠leaderId
				if ck.sToc[reply.LeaderId] == -1 { //但是本地还没初始化呢，那就往下一个发
					ck.changeNextSendId()
				} else { //本地还真知道，那下一个就发它所指定的localServerAddress
					ck.nextSendLocalId = ck.sToc[reply.LeaderId]
				}

			}
		case ErrWaitForRecover:
			// DPrintf("client [%d] [Get]:[Wait for leader recover]", ck.clientId)
			time.Sleep(time.Millisecond * 200)
		default:
			log.Fatalf("Client [%d] Get reply unknown err [%s](probaly not init)", ck.clientId, reply.Err)
		}

	}
}

// shared by Put and Append.
//
// you can send an RPC with code like this:
// ok := ck.servers[i].Call("KVServer."+op, &args, &reply)
//
// the types of args and reply (including whether they are pointers)
// must match the declared types of the RPC handler function's
// arguments. and reply must be passed as a pointer.
func (ck *Clerk) PutAppend(key string, value string, op string) {
	// You will have to modify this function.
	args := PutAppendArgs{
		Key:          key,
		Value:        value,
		Op:           op,
		ClientId:     ck.clientId,
		LatestOffset: ck.LatestOffset,
	}
	count := 0
	lastSendLocalId := -1
	for {

		if ck.nextSendLocalId == lastSendLocalId {
			count++
			if count > 5 {
				count = 0
				ck.changeNextSendId()
			}
		}

		// DPrintf("clinet [%d] [PutAppend]:send[%d] args[%v]", ck.clientId, ck.nextSendLocalId, args)
		reply := PutAppendReply{}
		ok := ck.servers[ck.nextSendLocalId].Call("KVServer.PutAppend", &args, &reply)

		//根据reply初始化一下本地server表

		lastSendLocalId = ck.nextSendLocalId
		if !ok {
			// DPrintf("clinet [%d] [PutAppend]:[lost] args[%v]", ck.clientId, args)
			//对面失联，那就换下一个继续发
			ck.changeNextSendId()
			continue
		}

		ck.sToc[reply.ServerId] = ck.nextSendLocalId

		switch reply.Err {
		case OK:
			ck.LatestOffset++
			// DPrintf("clinet [%d] [PutAppend]:[OK] args[%v] reply[%v]", ck.clientId, args, reply)
			return
		case ErrNoKey:
			// log.Fatalf("Client [%d] [PutAppend]:reply ErrNokey, but should not happend to putAndAppend args", ck.clientId)
		case ErrWrongLeader:
			// DPrintf("clinet [%d] [PutAppend]:[ErrWrong LeaderId][%d] get args[%v] reply[%v]", ck.clientId, ck.nextSendLocalId, args, reply)
			//对方也不知道leader
			if reply.LeaderId == -1 {
				//寻找下一个
				ck.changeNextSendId()
			} else {
				//记录对方返回的不可靠leaderId
				if ck.sToc[reply.LeaderId] == -1 { //但是本地还没初始化呢，那就往下一个发
					ck.changeNextSendId()
				} else { //本地还真知道，那下一个就发它所指定的localServerAddress
					ck.nextSendLocalId = ck.sToc[reply.LeaderId]
				}

			}
		default:
			log.Fatalf("Client [%d] [PutAppend]:reply unknown err [%s](probaly not init)", ck.clientId, reply.Err)
		}

	}
}

func (ck *Clerk) changeNextSendId() {
	ck.nextSendLocalId = (ck.nextSendLocalId + 1) % len(ck.servers)
}

func (ck *Clerk) Put(key string, value string) {
	ck.PutAppend(key, value, "Put")
}
func (ck *Clerk) Append(key string, value string) {
	ck.PutAppend(key, value, "Append")
}
