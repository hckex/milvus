// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package checkers

import (
	"context"
	"sort"
	"time"

	"github.com/samber/lo"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus/internal/querycoordv2/balance"
	"github.com/milvus-io/milvus/internal/querycoordv2/meta"
	. "github.com/milvus-io/milvus/internal/querycoordv2/params"
	"github.com/milvus-io/milvus/internal/querycoordv2/session"
	"github.com/milvus-io/milvus/internal/querycoordv2/task"
	"github.com/milvus-io/milvus/internal/querycoordv2/utils"
	"github.com/milvus-io/milvus/pkg/v2/common"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/proto/datapb"
	"github.com/milvus-io/milvus/pkg/v2/proto/querypb"
	"github.com/milvus-io/milvus/pkg/v2/util/funcutil"
)

const initialTargetVersion = int64(0)

type SegmentChecker struct {
	*checkerActivation
	meta            *meta.Meta
	dist            *meta.DistributionManager
	targetMgr       meta.TargetManagerInterface
	nodeMgr         *session.NodeManager
	getBalancerFunc GetBalancerFunc
}

func NewSegmentChecker(
	meta *meta.Meta,
	dist *meta.DistributionManager,
	targetMgr meta.TargetManagerInterface,
	nodeMgr *session.NodeManager,
	getBalancerFunc GetBalancerFunc,
) *SegmentChecker {
	return &SegmentChecker{
		checkerActivation: newCheckerActivation(),
		meta:              meta,
		dist:              dist,
		targetMgr:         targetMgr,
		nodeMgr:           nodeMgr,
		getBalancerFunc:   getBalancerFunc,
	}
}

func (c *SegmentChecker) ID() utils.CheckerType {
	return utils.SegmentChecker
}

func (c *SegmentChecker) Description() string {
	return "SegmentChecker checks the lack of segments, or some segments are redundant"
}

func (c *SegmentChecker) readyToCheck(ctx context.Context, collectionID int64) bool {
	metaExist := (c.meta.GetCollection(ctx, collectionID) != nil)
	targetExist := c.targetMgr.IsNextTargetExist(ctx, collectionID) || c.targetMgr.IsCurrentTargetExist(ctx, collectionID, common.AllPartitionsID)

	return metaExist && targetExist
}

func (c *SegmentChecker) Check(ctx context.Context) []task.Task {
	if !c.IsActive() {
		return nil
	}
	collectionIDs := c.meta.CollectionManager.GetAll(ctx)
	results := make([]task.Task, 0)
	for _, cid := range collectionIDs {
		if c.readyToCheck(ctx, cid) {
			replicas := c.meta.ReplicaManager.GetByCollection(ctx, cid)
			for _, r := range replicas {
				results = append(results, c.checkReplica(ctx, r)...)
			}
		}
	}

	// find already released segments which are not contained in target
	segments := c.dist.SegmentDistManager.GetByFilter()
	released := utils.FilterReleased(segments, collectionIDs)
	reduceTasks := c.createSegmentReduceTasks(ctx, released, meta.NilReplica, querypb.DataScope_Historical)
	task.SetReason("collection released", reduceTasks...)
	task.SetPriority(task.TaskPriorityNormal, reduceTasks...)
	results = append(results, reduceTasks...)

	// clean node which has been move out from replica
	for _, nodeInfo := range c.nodeMgr.GetAll() {
		nodeID := nodeInfo.ID()
		segmentsOnQN := c.dist.SegmentDistManager.GetByFilter(meta.WithNodeID(nodeID))
		collectionSegments := lo.GroupBy(segmentsOnQN, func(segment *meta.Segment) int64 { return segment.GetCollectionID() })
		for collectionID, segments := range collectionSegments {
			replica := c.meta.ReplicaManager.GetByCollectionAndNode(ctx, collectionID, nodeID)
			if replica == nil {
				reduceTasks := c.createSegmentReduceTasks(ctx, segments, meta.NilReplica, querypb.DataScope_Historical)
				task.SetReason("dirty segment exists", reduceTasks...)
				task.SetPriority(task.TaskPriorityNormal, reduceTasks...)
				results = append(results, reduceTasks...)
			}
		}
	}

	return results
}

func (c *SegmentChecker) checkReplica(ctx context.Context, replica *meta.Replica) []task.Task {
	ret := make([]task.Task, 0)

	// compare with targets to find the lack and redundancy of segments
	lacks, loadPriorities, redundancies := c.getSealedSegmentDiff(ctx, replica.GetCollectionID(), replica.GetID())
	tasks := c.createSegmentLoadTasks(c.getTraceCtx(ctx, replica.GetCollectionID()), lacks, loadPriorities, replica)
	task.SetReason("lacks of segment", tasks...)
	task.SetPriority(task.TaskPriorityNormal, tasks...)
	ret = append(ret, tasks...)

	redundancies = c.filterOutSegmentInUse(ctx, replica, redundancies)
	tasks = c.createSegmentReduceTasks(c.getTraceCtx(ctx, replica.GetCollectionID()), redundancies, replica, querypb.DataScope_Historical)
	task.SetReason("segment not exists in target", tasks...)
	task.SetPriority(task.TaskPriorityNormal, tasks...)
	ret = append(ret, tasks...)

	// compare inner dists to find repeated loaded segments
	redundancies = c.findRepeatedSealedSegments(ctx, replica.GetID())
	redundancies = c.filterOutExistedOnLeader(replica, redundancies)
	tasks = c.createSegmentReduceTasks(c.getTraceCtx(ctx, replica.GetCollectionID()), redundancies, replica, querypb.DataScope_Historical)
	task.SetReason("redundancies of segment", tasks...)
	// set deduplicate task priority to low, to avoid deduplicate task cancel balance task
	task.SetPriority(task.TaskPriorityLow, tasks...)
	ret = append(ret, tasks...)

	// compare with target to find the lack and redundancy of segments
	_, redundancies = c.getGrowingSegmentDiff(ctx, replica.GetCollectionID(), replica.GetID())
	tasks = c.createSegmentReduceTasks(c.getTraceCtx(ctx, replica.GetCollectionID()), redundancies, replica, querypb.DataScope_Streaming)
	task.SetReason("streaming segment not exists in target", tasks...)
	task.SetPriority(task.TaskPriorityNormal, tasks...)
	ret = append(ret, tasks...)

	return ret
}

// GetGrowingSegmentDiff get streaming segment diff between leader view and target
func (c *SegmentChecker) getGrowingSegmentDiff(ctx context.Context, collectionID int64,
	replicaID int64,
) (toLoad []*datapb.SegmentInfo, toRelease []*meta.Segment) {
	replica := c.meta.Get(ctx, replicaID)
	if replica == nil {
		log.Info("replica does not exist, skip it")
		return
	}

	log := log.Ctx(context.TODO()).WithRateGroup("qcv2.SegmentChecker", 1, 60).With(
		zap.Int64("collectionID", collectionID),
		zap.Int64("replicaID", replica.GetID()))

	delegatorList := c.dist.ChannelDistManager.GetByFilter(meta.WithReplica2Channel(replica))
	for _, d := range delegatorList {
		view := d.View
		targetVersion := c.targetMgr.GetCollectionTargetVersion(ctx, collectionID, meta.CurrentTarget)
		if view.TargetVersion != targetVersion {
			// before shard delegator update it's readable version, skip release segment
			log.RatedInfo(20, "before shard delegator update it's readable version, skip release segment",
				zap.String("channelName", view.Channel),
				zap.Int64("nodeID", view.ID),
				zap.Int64("leaderVersion", view.TargetVersion),
				zap.Int64("currentVersion", targetVersion),
			)
			continue
		}

		nextTargetExist := c.targetMgr.IsNextTargetExist(ctx, collectionID)
		nextTargetSegmentIDs := c.targetMgr.GetGrowingSegmentsByCollection(ctx, collectionID, meta.NextTarget)
		currentTargetSegmentIDs := c.targetMgr.GetGrowingSegmentsByCollection(ctx, collectionID, meta.CurrentTarget)
		currentTargetChannelMap := c.targetMgr.GetDmChannelsByCollection(ctx, collectionID, meta.CurrentTarget)

		// get segment which exist on leader view, but not on current target and next target
		for _, segment := range view.GrowingSegments {
			if !currentTargetSegmentIDs.Contain(segment.GetID()) && nextTargetExist && !nextTargetSegmentIDs.Contain(segment.GetID()) {
				if channel, ok := currentTargetChannelMap[segment.InsertChannel]; ok {
					timestampInSegment := segment.GetStartPosition().GetTimestamp()
					timestampInTarget := channel.GetSeekPosition().GetTimestamp()
					// release growing segment if in dropped segment list
					if funcutil.SliceContain(channel.GetDroppedSegmentIds(), segment.GetID()) {
						log.Info("growing segment exists in dropped segment list, release it", zap.Int64("segmentID", segment.GetID()))
						toRelease = append(toRelease, segment)
						continue
					}
					// filter toRelease which seekPosition is newer than next target dmChannel
					if timestampInSegment < timestampInTarget {
						log.Info("growing segment not exist in target, so release it",
							zap.Int64("segmentID", segment.GetID()),
						)
						toRelease = append(toRelease, segment)
					}
				}
			}
		}
	}

	return
}

// GetSealedSegmentDiff get historical segment diff between target and dist
func (c *SegmentChecker) getSealedSegmentDiff(
	ctx context.Context,
	collectionID int64,
	replicaID int64,
) (toLoad []*datapb.SegmentInfo, loadPriorities []commonpb.LoadPriority, toRelease []*meta.Segment) {
	replica := c.meta.Get(ctx, replicaID)
	if replica == nil {
		log.Info("replica does not exist, skip it")
		return
	}
	dist := c.dist.SegmentDistManager.GetByFilter(meta.WithCollectionID(replica.GetCollectionID()), meta.WithReplica(replica))
	sort.Slice(dist, func(i, j int) bool {
		return dist[i].Version < dist[j].Version
	})
	distMap := make(map[int64]int64)
	for _, s := range dist {
		distMap[s.GetID()] = s.Node
	}

	isSegmentLack := func(segment *datapb.SegmentInfo) bool {
		_, existInDist := distMap[segment.ID]
		return !existInDist
	}

	nextTargetExist := c.targetMgr.IsNextTargetExist(ctx, collectionID)
	nextTargetMap := c.targetMgr.GetSealedSegmentsByCollection(ctx, collectionID, meta.NextTarget)
	currentTargetMap := c.targetMgr.GetSealedSegmentsByCollection(ctx, collectionID, meta.CurrentTarget)
	// Segment which exist on next target, but not on dist
	for _, segment := range nextTargetMap {
		if isSegmentLack(segment) {
			_, existOnCurrent := currentTargetMap[segment.GetID()]
			if existOnCurrent {
				loadPriorities = append(loadPriorities, commonpb.LoadPriority_HIGH)
			} else {
				loadPriorities = append(loadPriorities, replica.LoadPriority())
			}
			toLoad = append(toLoad, segment)
		}
	}

	// get segment which exist on dist, but not on current target and next target
	for _, segment := range dist {
		_, existOnCurrent := currentTargetMap[segment.GetID()]
		_, existOnNext := nextTargetMap[segment.GetID()]

		// l0 segment should be release with channel together
		if !existOnNext && nextTargetExist && !existOnCurrent {
			toRelease = append(toRelease, segment)
		}
	}

	return
}

func (c *SegmentChecker) findRepeatedSealedSegments(ctx context.Context, replicaID int64) []*meta.Segment {
	segments := make([]*meta.Segment, 0)
	replica := c.meta.Get(ctx, replicaID)
	if replica == nil {
		log.Info("replica does not exist, skip it")
		return segments
	}
	dist := c.dist.SegmentDistManager.GetByFilter(meta.WithCollectionID(replica.GetCollectionID()), meta.WithReplica(replica))
	versions := make(map[int64]*meta.Segment)
	for _, s := range dist {
		maxVer, ok := versions[s.GetID()]
		if !ok {
			versions[s.GetID()] = s
			continue
		}
		if maxVer.Version <= s.Version {
			segments = append(segments, maxVer)
			versions[s.GetID()] = s
		} else {
			segments = append(segments, s)
		}
	}

	return segments
}

// for duplicated segment, we should release the one which is not serving on leader
func (c *SegmentChecker) filterOutExistedOnLeader(replica *meta.Replica, segments []*meta.Segment) []*meta.Segment {
	notServing := make([]*meta.Segment, 0, len(segments))
	delegatorList := c.dist.ChannelDistManager.GetByFilter(meta.WithReplica2Channel(replica))
	ch2DelegatorList := lo.GroupBy(delegatorList, func(d *meta.DmChannel) string {
		return d.View.Channel
	})
	for _, s := range segments {
		delegatorList := ch2DelegatorList[s.GetInsertChannel()]
		if len(delegatorList) == 0 {
			continue
		}

		servingOnLeader := false
		for _, delegator := range delegatorList {
			segInView, ok := delegator.View.Segments[s.GetID()]
			if ok && segInView.NodeID == s.Node {
				servingOnLeader = true
				break
			}
		}

		if !servingOnLeader {
			notServing = append(notServing, s)
		}
	}
	return notServing
}

// for sealed segment which doesn't exist in target, we should release it after delegator has updated to latest readable version
func (c *SegmentChecker) filterOutSegmentInUse(ctx context.Context, replica *meta.Replica, segments []*meta.Segment) []*meta.Segment {
	notUsed := make([]*meta.Segment, 0, len(segments))
	delegatorList := c.dist.ChannelDistManager.GetByFilter(meta.WithReplica2Channel(replica))
	ch2DelegatorList := lo.GroupBy(delegatorList, func(d *meta.DmChannel) string {
		return d.View.Channel
	})
	for _, s := range segments {
		currentTargetVersion := c.targetMgr.GetCollectionTargetVersion(ctx, s.CollectionID, meta.CurrentTarget)
		partition := c.meta.CollectionManager.GetPartition(ctx, s.PartitionID)

		delegatorList := ch2DelegatorList[s.GetInsertChannel()]
		if len(delegatorList) == 0 {
			continue
		}

		stillInUseByDelegator := false
		// if delegator has valid target version, and before it update to latest readable version, skip release it's sealed segment
		for _, delegator := range delegatorList {
			// Notice: if syncTargetVersion stuck, segment on delegator won't be released
			readableVersionNotUpdate := delegator.View.TargetVersion != initialTargetVersion && delegator.View.TargetVersion < currentTargetVersion
			if partition != nil && readableVersionNotUpdate {
				// leader view version hasn't been updated, segment maybe still in use
				stillInUseByDelegator = true
				break
			}
		}

		if !stillInUseByDelegator {
			notUsed = append(notUsed, s)
		}
	}
	return notUsed
}

func (c *SegmentChecker) createSegmentLoadTasks(ctx context.Context, segments []*datapb.SegmentInfo, loadPriorities []commonpb.LoadPriority, replica *meta.Replica) []task.Task {
	if len(segments) == 0 {
		return nil
	}
	priorityMap := make(map[int64]commonpb.LoadPriority)
	for i, s := range segments {
		priorityMap[s.GetID()] = loadPriorities[i]
	}

	shardSegments := lo.GroupBy(segments, func(s *datapb.SegmentInfo) string {
		return s.GetInsertChannel()
	})

	plans := make([]balance.SegmentAssignPlan, 0)
	for shard, segments := range shardSegments {
		// if channel is not subscribed yet, skip load segments
		leader := c.dist.ChannelDistManager.GetShardLeader(shard, replica)
		if leader == nil {
			continue
		}

		rwNodes := replica.GetChannelRWNodes(shard)
		if len(rwNodes) == 0 {
			rwNodes = replica.GetRWNodes()
		}

		segmentInfos := lo.Map(segments, func(s *datapb.SegmentInfo, _ int) *meta.Segment {
			return &meta.Segment{
				SegmentInfo: s,
			}
		})
		shardPlans := c.getBalancerFunc().AssignSegment(ctx, replica.GetCollectionID(), segmentInfos, rwNodes, true)
		for i := range shardPlans {
			shardPlans[i].Replica = replica
			shardPlans[i].LoadPriority = priorityMap[shardPlans[i].Segment.GetID()]
		}
		plans = append(plans, shardPlans...)
	}

	return balance.CreateSegmentTasksFromPlans(ctx, c.ID(), Params.QueryCoordCfg.SegmentTaskTimeout.GetAsDuration(time.Millisecond), plans)
}

func (c *SegmentChecker) createSegmentReduceTasks(ctx context.Context, segments []*meta.Segment, replica *meta.Replica, scope querypb.DataScope) []task.Task {
	ret := make([]task.Task, 0, len(segments))
	for _, s := range segments {
		action := task.NewSegmentActionWithScope(s.Node, task.ActionTypeReduce, s.GetInsertChannel(), s.GetID(), scope, int(s.GetNumOfRows()))
		task, err := task.NewSegmentTask(
			ctx,
			Params.QueryCoordCfg.SegmentTaskTimeout.GetAsDuration(time.Millisecond),
			c.ID(),
			s.GetCollectionID(),
			replica,
			replica.LoadPriority(),
			action,
		)
		if err != nil {
			log.Warn("create segment reduce task failed",
				zap.Int64("collection", s.GetCollectionID()),
				zap.Int64("replica", replica.GetID()),
				zap.String("channel", s.GetInsertChannel()),
				zap.Int64("from", s.Node),
				zap.Error(err),
			)
			continue
		}

		ret = append(ret, task)
	}
	return ret
}

func (c *SegmentChecker) getTraceCtx(ctx context.Context, collectionID int64) context.Context {
	coll := c.meta.GetCollection(ctx, collectionID)
	if coll == nil || coll.LoadSpan == nil {
		return ctx
	}

	return trace.ContextWithSpan(ctx, coll.LoadSpan)
}
