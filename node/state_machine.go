package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/absolute8511/ZanRedisDB/common"
	"github.com/absolute8511/ZanRedisDB/raft/raftpb"
	"github.com/absolute8511/ZanRedisDB/rockredis"
	"github.com/absolute8511/redcon"
	"github.com/coreos/etcd/pkg/wait"
)

type StateMachine interface {
	ApplyRaftRequest(req BatchInternalRaftRequest, term uint64, index uint64, stop chan struct{}) bool
	GetSnapshot(term uint64, index uint64) (*KVSnapInfo, error)
	RestoreFromSnapshot(startup bool, raftSnapshot raftpb.Snapshot, stop chan struct{}) error
	Destroy()
	CleanData() error
	Optimize()
	GetStats() common.NamespaceStats
	Start() error
	Close()
	CheckExpiredData(buffer common.ExpiredDataBuffer, stop chan struct{}) error
}

func NewStateMachine(opts *KVOptions, machineConfig MachineConfig, localID uint64,
	fullNS string, clusterInfo common.IClusterInfo) (StateMachine, error) {
	if machineConfig.LearnerRole == "" {
		if machineConfig.StateMachineType == "empty_sm" {
			return &emptySM{}, nil
		}
		return NewKVStoreSM(opts, machineConfig, localID, fullNS, clusterInfo)
	} else if machineConfig.LearnerRole == common.LearnerRoleLogSyncer {
		return NewLogSyncerSM(opts, machineConfig, localID, fullNS, clusterInfo)
	} else {
		return nil, errors.New("unknown learner role")
	}
}

type emptySM struct {
	w wait.Wait
}

func (esm *emptySM) ApplyRaftRequest(reqList BatchInternalRaftRequest, term uint64, index uint64, stop chan struct{}) bool {
	for _, req := range reqList.Reqs {
		reqID := req.Header.ID
		esm.w.Trigger(reqID, nil)
	}
	return false
}

func (esm *emptySM) GetSnapshot(term uint64, index uint64) (*KVSnapInfo, error) {
	var s KVSnapInfo
	return &s, nil
}
func (esm *emptySM) RestoreFromSnapshot(startup bool, raftSnapshot raftpb.Snapshot, stop chan struct{}) error {
	return nil
}
func (esm *emptySM) Destroy() {

}
func (esm *emptySM) CleanData() error {
	return nil
}
func (esm *emptySM) Optimize() {

}
func (esm *emptySM) GetStats() common.NamespaceStats {
	return common.NamespaceStats{}
}
func (esm *emptySM) Start() error {
	return nil
}
func (esm *emptySM) Close() {

}
func (esm *emptySM) CheckExpiredData(buffer common.ExpiredDataBuffer, stop chan struct{}) error {
	return nil
}

type kvStoreSM struct {
	fullName      string
	store         *KVStore
	clusterInfo   common.IClusterInfo
	fullNS        string
	machineConfig MachineConfig
	ID            uint64
	dbWriteStats  common.WriteStats
	w             wait.Wait
	router        *common.SMCmdRouter
	stopping      int32
}

func NewKVStoreSM(opts *KVOptions, machineConfig MachineConfig, localID uint64, ns string,
	clusterInfo common.IClusterInfo) (StateMachine, error) {
	store, err := NewKVStore(opts)
	if err != nil {
		return nil, err
	}
	sm := &kvStoreSM{
		fullNS:        ns,
		machineConfig: machineConfig,
		ID:            localID,
		clusterInfo:   clusterInfo,
		store:         store,
		router:        common.NewSMCmdRouter(),
	}
	sm.registerHandlers()
	return sm, nil
}

func (kvsm *kvStoreSM) Debugf(f string, args ...interface{}) {
	msg := fmt.Sprintf(f, args...)
	nodeLog.DebugDepth(1, fmt.Sprintf("%v: %s", kvsm.fullNS, msg))
}

func (kvsm *kvStoreSM) Infof(f string, args ...interface{}) {
	msg := fmt.Sprintf(f, args...)
	nodeLog.InfoDepth(1, fmt.Sprintf("%v: %s", kvsm.fullNS, msg))
}

func (kvsm *kvStoreSM) Errorf(f string, args ...interface{}) {
	msg := fmt.Sprintf(f, args...)
	nodeLog.ErrorDepth(1, fmt.Sprintf("%v: %s", kvsm.fullNS, msg))
}

func (kvsm *kvStoreSM) Start() error {
	return nil
}

func (kvsm *kvStoreSM) Close() {
	if !atomic.CompareAndSwapInt32(&kvsm.stopping, 0, 1) {
		return
	}
	kvsm.store.Close()
}

func (kvsm *kvStoreSM) Optimize() {
	kvsm.store.CompactRange()
}

func (kvsm *kvStoreSM) GetDBInternalStats() string {
	return kvsm.store.GetStatistics()
}

func (kvsm *kvStoreSM) GetStats() common.NamespaceStats {
	tbs := kvsm.store.GetTables()
	var ns common.NamespaceStats
	ns.InternalStats = kvsm.store.GetInternalStatus()
	ns.DBWriteStats = kvsm.dbWriteStats.Copy()

	for t := range tbs {
		cnt, err := kvsm.store.GetTableKeyCount(t)
		if err != nil {
			continue
		}
		var ts common.TableStats
		ts.Name = string(t)
		ts.KeyNum = cnt
		ns.TStats = append(ns.TStats, ts)
	}
	return ns
}

func (kvsm *kvStoreSM) CleanData() error {
	return kvsm.store.CleanData()
}

func (kvsm *kvStoreSM) Destroy() {
	kvsm.store.Destroy()
}

func (kvsm *kvStoreSM) CheckExpiredData(buffer common.ExpiredDataBuffer, stop chan struct{}) error {
	return kvsm.store.CheckExpiredData(buffer, stop)
}

func (kvsm *kvStoreSM) GetSnapshot(term uint64, index uint64) (*KVSnapInfo, error) {
	var si KVSnapInfo
	// use the rocksdb backup/checkpoint interface to backup data
	si.BackupInfo = kvsm.store.Backup(term, index)
	if si.BackupInfo == nil {
		return nil, errors.New("failed to begin backup: maybe too much backup running")
	}
	si.WaitReady()
	return &si, nil
}

func checkLocalBackup(store *KVStore, rs raftpb.Snapshot) (bool, error) {
	var si KVSnapInfo
	err := json.Unmarshal(rs.Data, &si)
	if err != nil {
		return false, err
	}
	return store.IsLocalBackupOK(rs.Metadata.Term, rs.Metadata.Index)
}

func prepareSnapshotForStore(store *KVStore, machineConfig MachineConfig,
	clusterInfo common.IClusterInfo, fullNS string,
	localID uint64, stopChan chan struct{},
	raftSnapshot raftpb.Snapshot, retry int) error {

	hasBackup, _ := checkLocalBackup(store, raftSnapshot)
	if hasBackup {
		return nil
	}
	if clusterInfo == nil {
		return errors.New("cluster info is not available.")
	}
	syncAddr, syncDir := GetValidBackupInfo(machineConfig, clusterInfo, fullNS, localID, stopChan, raftSnapshot, retry)
	if syncAddr == "" && syncDir == "" {
		return errors.New("no backup available from others")
	}
	// copy backup data from the remote leader node, and recovery backup from it
	// if local has some old backup data, we should use rsync to sync the data file
	// use the rocksdb backup/checkpoint interface to backup data
	err := common.RunFileSync(syncAddr,
		path.Join(rockredis.GetBackupDir(syncDir),
			rockredis.GetCheckpointDir(raftSnapshot.Metadata.Term, raftSnapshot.Metadata.Index)),
		store.GetBackupDir())

	return err
}

func GetValidBackupInfo(machineConfig MachineConfig,
	clusterInfo common.IClusterInfo, fullNS string,
	localID uint64, stopChan chan struct{},
	raftSnapshot raftpb.Snapshot, retryIndex int) (string, string) {
	// we need find the right backup data match with the raftsnapshot
	// for each cluster member, it need check the term+index and the backup meta to
	// make sure the data is valid
	syncAddr := ""
	syncDir := ""
	h := machineConfig.BroadcastAddr

	innerRetry := 0
	var snapSyncInfoList []common.SnapshotSyncInfo
	var err error
	for innerRetry < 3 {
		innerRetry++
		snapSyncInfoList, err = clusterInfo.GetSnapshotSyncInfo(fullNS)
		if err != nil {
			nodeLog.Infof("%v get snapshot info failed: %v", fullNS, err)
			select {
			case <-stopChan:
				break
			case <-time.After(time.Second):
			}
		} else {
			break
		}
	}

	nodeLog.Infof("%v current cluster raft nodes info: %v", fullNS, snapSyncInfoList)
	syncAddrList := make([]string, 0)
	syncDirList := make([]string, 0)
	for _, ssi := range snapSyncInfoList {
		if ssi.ReplicaID == localID {
			continue
		}

		c := http.Client{Transport: common.NewDeadlineTransport(time.Second)}
		body, _ := raftSnapshot.Marshal()
		uri := "http://" + ssi.RemoteAddr + ":" +
			ssi.HttpAPIPort + common.APICheckBackup + "/" + fullNS
		req, _ := http.NewRequest("GET", uri, bytes.NewBuffer(body))
		rsp, err := c.Do(req)
		if err != nil {
			nodeLog.Infof("request %v error: %v", uri, err)
			continue
		}
		rsp.Body.Close()
		if rsp.StatusCode != http.StatusOK {
			nodeLog.Infof("request %v error: %v", uri, rsp)
			continue
		}
		if ssi.RemoteAddr == h {
			if ssi.DataRoot == machineConfig.DataRootDir {
				// the leader is old mine, try find another leader
				nodeLog.Infof("data dir can not be same if on local: %v, %v", ssi, machineConfig)
				continue
			}
			// local node with different directory
			syncAddrList = append(syncAddrList, "")
			syncDirList = append(syncDirList, path.Join(ssi.DataRoot, fullNS))
		} else {
			// for remote snapshot, we do rsync from remote module
			syncAddrList = append(syncAddrList, ssi.RemoteAddr)
			syncDirList = append(syncDirList, path.Join(ssi.RsyncModule, fullNS))
		}
	}
	if len(syncAddrList) > 0 {
		syncAddr = syncAddrList[retryIndex%len(syncAddrList)]
		syncDir = syncDirList[retryIndex%len(syncDirList)]
	}
	nodeLog.Infof("%v should recovery from : %v, %v", fullNS, syncAddr, syncDir)
	return syncAddr, syncDir
}

func (kvsm *kvStoreSM) RestoreFromSnapshot(startup bool, raftSnapshot raftpb.Snapshot, stop chan struct{}) error {
	// while startup we can use the local snapshot to restart,
	// but while running, we should install the leader's snapshot,
	// so we need remove local and sync from leader
	retry := 0
	for retry < 3 {
		err := prepareSnapshotForStore(kvsm.store, kvsm.machineConfig, kvsm.clusterInfo, kvsm.fullNS,
			kvsm.ID, stop, raftSnapshot, retry)
		if err != nil {
			kvsm.Infof("failed to prepare snapshot: %v", err)
		} else {
			err = kvsm.store.Restore(raftSnapshot.Metadata.Term, raftSnapshot.Metadata.Index)
			if err == nil {
				return nil
			}
		}
		retry++
		kvsm.Infof("failed to restore snapshot: %v", err)
		select {
		case <-stop:
			return err
		case <-time.After(time.Second):
		}
	}
	return errors.New("failed to restore from snapshot")
}

func (kvsm *kvStoreSM) ApplyRaftRequest(reqList BatchInternalRaftRequest, term uint64, index uint64, stop chan struct{}) bool {
	forceBackup := false
	start := time.Now()
	batching := false
	var batchReqIDList []uint64
	var batchReqRspList []interface{}
	var batchStart time.Time
	dupCheckMap := make(map[string]bool, len(reqList.Reqs))
	lastBatchCmd := ""
	ts := reqList.Timestamp
	if reqList.Type == FromClusterSyncer {
		if nodeLog.Level() >= common.LOG_DETAIL {
			kvsm.Debugf("recv write from cluster syncer at (%v-%v): %v", term, index, reqList.String())
		}
		// TODO: check original term and index
		// if less or equal than last, we need log and skip

		// TODO: here we need compare the key timestamp in this cluster and the timestamp from raft request to handle
		// the conflict change between two cluster.
		//
	}
	for reqIndex, req := range reqList.Reqs {
		reqTs := ts
		if reqTs == 0 {
			reqTs = req.Header.Timestamp
		}
		reqID := req.Header.ID
		if req.Header.DataType == 0 {
			cmd, err := redcon.Parse(req.Data)
			if err != nil {
				kvsm.w.Trigger(reqID, err)
			} else {
				cmdStart := time.Now()
				cmdName := strings.ToLower(string(cmd.Args[0]))
				_, pk, _ := common.ExtractNamesapce(cmd.Args[1])
				_, ok := dupCheckMap[string(pk)]
				handled := false
				// TODO: table counter can be batched???
				if kvsm.store.IsBatchableWrite(cmdName) &&
					len(batchReqIDList) < maxBatchCmdNum &&
					!ok {
					if !batching {
						err := kvsm.store.BeginBatchWrite()
						if err != nil {
							kvsm.Infof("begin batch command %v failed: %v, %v", cmdName, string(cmd.Raw), err)
							kvsm.w.Trigger(reqID, err)
							continue
						}
						batchStart = time.Now()
						batching = true
					}
					handled = true
					lastBatchCmd = cmdName
					h, ok := kvsm.router.GetInternalCmdHandler(cmdName)
					if !ok {
						kvsm.Infof("unsupported redis command: %v", cmdName)
						kvsm.w.Trigger(reqID, common.ErrInvalidCommand)
					} else {
						if pk != nil {
							dupCheckMap[string(pk)] = true
						}
						v, err := h(cmd, reqTs)
						if err != nil {
							kvsm.Infof("redis command %v error: %v, cmd: %v", cmdName, err, cmd)
							kvsm.w.Trigger(reqID, err)
							continue
						}
						if nodeLog.Level() > common.LOG_DETAIL {
							kvsm.Infof("batching write command: %v", string(cmd.Raw))
						}
						batchReqIDList = append(batchReqIDList, reqID)
						batchReqRspList = append(batchReqRspList, v)
						kvsm.dbWriteStats.UpdateSizeStats(int64(len(cmd.Raw)))
					}
					if nodeLog.Level() > common.LOG_DETAIL {
						kvsm.Infof("batching redis command: %v", cmdName)
					}
					if reqIndex < len(reqList.Reqs)-1 {
						continue
					}
				}
				if batching {
					batching = false
					batchReqIDList, batchReqRspList, dupCheckMap = kvsm.processBatching(lastBatchCmd, reqList, batchStart,
						batchReqIDList, batchReqRspList, dupCheckMap)
				}
				if handled {
					continue
				}

				h, ok := kvsm.router.GetInternalCmdHandler(cmdName)
				if !ok {
					kvsm.Infof("unsupported redis command: %v", cmd)
					kvsm.w.Trigger(reqID, common.ErrInvalidCommand)
				} else {
					v, err := h(cmd, reqTs)
					cmdCost := time.Since(cmdStart)
					if cmdCost >= time.Second || nodeLog.Level() > common.LOG_DETAIL ||
						(nodeLog.Level() >= common.LOG_DEBUG && cmdCost > time.Millisecond*100) {
						kvsm.Infof("slow write command: %v, cost: %v", string(cmd.Raw), cmdCost)
					}
					kvsm.dbWriteStats.UpdateWriteStats(int64(len(cmd.Raw)), cmdCost.Nanoseconds()/1000)
					// write the future response or error
					if err != nil {
						kvsm.Infof("redis command %v error: %v, cmd: %v", cmdName, err, string(cmd.Raw))
						kvsm.w.Trigger(reqID, err)
					} else {
						kvsm.w.Trigger(reqID, v)
					}
				}
			}
		} else {
			if batching {
				batching = false
				batchReqIDList, batchReqRspList, dupCheckMap = kvsm.processBatching(lastBatchCmd, reqList, batchStart,
					batchReqIDList, batchReqRspList, dupCheckMap)
			}
			if req.Header.DataType == int32(HTTPReq) {
				var p httpProposeData
				err := json.Unmarshal(req.Data, &p)
				if err != nil {
					kvsm.Infof("failed to unmarshal http propose: %v", req.String())
					kvsm.w.Trigger(reqID, err)
				}
				if p.ProposeOp == HttpProposeOp_Backup {
					kvsm.Infof("got force backup request")
					forceBackup = true
					kvsm.w.Trigger(reqID, nil)
				} else {
					kvsm.w.Trigger(reqID, errUnknownData)
				}
			} else if req.Header.DataType == int32(SchemaChangeReq) {
				kvsm.Infof("handle schema change: %v", string(req.Data))
				var sc SchemaChange
				err := sc.Unmarshal(req.Data)
				if err != nil {
					kvsm.Infof("schema data error: %v, %v", string(req.Data), err)
					kvsm.w.Trigger(reqID, err)
				} else {
					err = kvsm.handleSchemaUpdate(sc)
					kvsm.w.Trigger(reqID, err)
				}
			} else {
				kvsm.w.Trigger(reqID, errUnknownData)
			}
		}
	}
	// TODO: add test case for this
	if batching {
		batching = false
		batchReqIDList, batchReqRspList, dupCheckMap = kvsm.processBatching(lastBatchCmd, reqList, batchStart,
			batchReqIDList, batchReqRspList, dupCheckMap)
	}
	for _, req := range reqList.Reqs {
		if kvsm.w.IsRegistered(req.Header.ID) {
			kvsm.Infof("missing process request: %v", req.String())
			kvsm.w.Trigger(req.Header.ID, errUnknownData)
		}
	}

	cost := time.Since(start)
	if cost >= proposeTimeout/2 {
		kvsm.Infof("slow for batch write db: %v, cost %v", len(reqList.Reqs), cost)
	}
	if reqList.Type == FromClusterSyncer {
		// TODO: update the orig term-index
	}
	// used for grpc raft proposal, will notify that all the raft logs in this batch is done.
	if reqList.ReqId > 0 {
		kvsm.w.Trigger(reqList.ReqId, nil)
	}
	return forceBackup
}

// return if configure changed and whether need force backup
func (kvsm *kvStoreSM) processBatching(cmdName string, reqList BatchInternalRaftRequest, batchStart time.Time, batchReqIDList []uint64, batchReqRspList []interface{},
	dupCheckMap map[string]bool) ([]uint64, []interface{}, map[string]bool) {
	err := kvsm.store.CommitBatchWrite()
	dupCheckMap = make(map[string]bool, len(reqList.Reqs))
	batchCost := time.Since(batchStart)
	if nodeLog.Level() >= common.LOG_DETAIL {
		kvsm.Infof("batching command number: %v", len(batchReqIDList))
	}
	// write the future response or error
	for idx, rid := range batchReqIDList {
		if err != nil {
			kvsm.w.Trigger(rid, err)
		} else {
			kvsm.w.Trigger(rid, batchReqRspList[idx])
		}
	}
	if batchCost >= time.Second || (nodeLog.Level() >= common.LOG_DEBUG && batchCost > time.Millisecond*100) {
		kvsm.Infof("slow batch write command: %v, batch: %v, cost: %v",
			cmdName, len(batchReqIDList), batchCost)
	}
	if len(batchReqIDList) > 0 {
		kvsm.dbWriteStats.UpdateLatencyStats(batchCost.Nanoseconds() / int64(len(batchReqIDList)) / 1000)
	}
	batchReqIDList = batchReqIDList[:0]
	batchReqRspList = batchReqRspList[:0]
	return batchReqIDList, batchReqRspList, dupCheckMap
}