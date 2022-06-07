// Copyright 2019 TiKV Project Authors.
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

package schedule

import (
	"context"

	"github.com/tikv/pd/pkg/cache"
	"github.com/tikv/pd/pkg/errs"
	"github.com/tikv/pd/server/config"
	"github.com/tikv/pd/server/core"
	"github.com/tikv/pd/server/schedule/checker"
	"github.com/tikv/pd/server/schedule/labeler"
	"github.com/tikv/pd/server/schedule/operator"
	"github.com/tikv/pd/server/schedule/opt"
	"github.com/tikv/pd/server/schedule/placement"
)

// DefaultCacheSize is the default length of waiting list.
const DefaultCacheSize = 1000

// CheckerController is used to manage all checkers.
type CheckerController struct {
	cluster           opt.Cluster
	opts              *config.PersistOptions
	opController      *OperatorController
	learnerChecker    *checker.LearnerChecker
	replicaChecker    *checker.ReplicaChecker
	ruleChecker       *checker.RuleChecker
	splitChecker      *checker.SplitChecker
	mergeChecker      *checker.MergeChecker
	jointStateChecker *checker.JointStateChecker
	priorityInspector *checker.PriorityInspector
	regionWaitingList cache.Cache
}

// NewCheckerController create a new CheckerController.
// TODO: isSupportMerge should be removed.
func NewCheckerController(ctx context.Context, cluster opt.Cluster, ruleManager *placement.RuleManager, labeler *labeler.RegionLabeler, opController *OperatorController) *CheckerController {
	regionWaitingList := cache.NewDefaultCache(DefaultCacheSize)
	return &CheckerController{
		cluster:           cluster,
		opts:              cluster.GetOpts(),
		opController:      opController,
		learnerChecker:    checker.NewLearnerChecker(cluster),
		replicaChecker:    checker.NewReplicaChecker(cluster, regionWaitingList),
		ruleChecker:       checker.NewRuleChecker(cluster, ruleManager, regionWaitingList),
		splitChecker:      checker.NewSplitChecker(cluster, ruleManager, labeler),
		mergeChecker:      checker.NewMergeChecker(ctx, cluster),
		jointStateChecker: checker.NewJointStateChecker(cluster),
		priorityInspector: checker.NewPriorityInspector(cluster),
		regionWaitingList: regionWaitingList,
	}
}

// CheckRegion will check the region and add a new operator if needed.
func (c *CheckerController) CheckRegion(region *core.RegionInfo) []*operator.Operator {
	// If PD has restarted, it need to check learners added before and promote them.
	// Don't check isRaftLearnerEnabled cause it maybe disable learner feature but there are still some learners to promote.
	opController := c.opController

	if op := c.jointStateChecker.Check(region); op != nil {
		return []*operator.Operator{op}
	}

	if op := c.splitChecker.Check(region); op != nil {
		return []*operator.Operator{op}
	}

	if c.opts.IsPlacementRulesEnabled() {
		fit := c.priorityInspector.Inspect(region)
		if op := c.ruleChecker.CheckWithFit(region, fit); op != nil {
			if opController.OperatorCount(operator.OpReplica) < c.opts.GetReplicaScheduleLimit() {
				return []*operator.Operator{op}
			}
			operator.OperatorLimitCounter.WithLabelValues(c.ruleChecker.GetType(), operator.OpReplica.String()).Inc()
			c.regionWaitingList.Put(region.GetID(), nil)
		}
	} else {
		if op := c.learnerChecker.Check(region); op != nil {
			return []*operator.Operator{op}
		}
		if op := c.replicaChecker.Check(region); op != nil {
			if opController.OperatorCount(operator.OpReplica) < c.opts.GetReplicaScheduleLimit() {
				return []*operator.Operator{op}
			}
			operator.OperatorLimitCounter.WithLabelValues(c.replicaChecker.GetType(), operator.OpReplica.String()).Inc()
			c.regionWaitingList.Put(region.GetID(), nil)
		}
	}

	if c.mergeChecker != nil {
		allowed := opController.OperatorCount(operator.OpMerge) < c.opts.GetMergeScheduleLimit()
		if !allowed {
			operator.OperatorLimitCounter.WithLabelValues(c.mergeChecker.GetType(), operator.OpMerge.String()).Inc()
		} else {
			if ops := c.mergeChecker.Check(region); ops != nil {
				// It makes sure that two operators can be added successfully altogether.
				return ops
			}
		}
	}
	return nil
}

// GetMergeChecker returns the merge checker.
func (c *CheckerController) GetMergeChecker() *checker.MergeChecker {
	return c.mergeChecker
}

// GetRuleChecker returns the rule checker.
func (c *CheckerController) GetRuleChecker() *checker.RuleChecker {
	return c.ruleChecker
}

// GetWaitingRegions returns the regions in the waiting list.
func (c *CheckerController) GetWaitingRegions() []*cache.Item {
	return c.regionWaitingList.Elems()
}

// AddWaitingRegion returns the regions in the waiting list.
func (c *CheckerController) AddWaitingRegion(region *core.RegionInfo) {
	c.regionWaitingList.Put(region.GetID(), nil)
}

// RemoveWaitingRegion removes the region from the waiting list.
func (c *CheckerController) RemoveWaitingRegion(id uint64) {
	c.regionWaitingList.Remove(id)
}

// GetPriorityRegions returns the region in priority queue
func (c *CheckerController) GetPriorityRegions() []uint64 {
	return c.priorityInspector.GetPriorityRegions()
}

// RemovePriorityRegions removes priority region from priority queue
func (c *CheckerController) RemovePriorityRegions(id uint64) {
	c.priorityInspector.RemovePriorityRegion(id)
}

<<<<<<< HEAD:server/schedule/checker_controller.go
=======
// AddSuspectRegions adds regions to suspect list.
func (c *Controller) AddSuspectRegions(regionIDs ...uint64) {
	for _, regionID := range regionIDs {
		c.suspectRegions.Put(regionID, nil)
	}
}

// GetSuspectRegions gets all suspect regions.
func (c *Controller) GetSuspectRegions() []uint64 {
	return c.suspectRegions.GetAllID()
}

// RemoveSuspectRegion removes region from suspect list.
func (c *Controller) RemoveSuspectRegion(id uint64) {
	c.suspectRegions.Remove(id)
}

// AddSuspectKeyRange adds the key range with the its ruleID as the key
// The instance of each keyRange is like following format:
// [2][]byte: start key/end key
func (c *Controller) AddSuspectKeyRange(start, end []byte) {
	c.suspectKeyRanges.Put(keyutil.BuildKeyRangeKey(start, end), [2][]byte{start, end})
}

// PopOneSuspectKeyRange gets one suspect keyRange group.
// it would return value and true if pop success, or return empty [][2][]byte and false
// if suspectKeyRanges couldn't pop keyRange group.
func (c *Controller) PopOneSuspectKeyRange() ([2][]byte, bool) {
	_, value, success := c.suspectKeyRanges.Pop()
	if !success {
		return [2][]byte{}, false
	}
	v, ok := value.([2][]byte)
	if !ok {
		return [2][]byte{}, false
	}
	return v, true
}

// ClearSuspectKeyRanges clears the suspect keyRanges, only for unit test
func (c *Controller) ClearSuspectKeyRanges() {
	c.suspectKeyRanges.Clear()
}

// IsPendingRegion returns true if the given region is in the pending list.
func (c *Controller) IsPendingRegion(regionID uint64) bool {
	_, exist := c.ruleChecker.pendingList.Get(regionID)
	return exist
}

>>>>>>> e19dc71ac (*: fix the wrong pending status (#5080)):server/schedule/checker/checker_controller.go
// GetPauseController returns pause controller of the checker
func (c *CheckerController) GetPauseController(name string) (*checker.PauseController, error) {
	switch name {
	case "learner":
		return &c.learnerChecker.PauseController, nil
	case "replica":
		return &c.replicaChecker.PauseController, nil
	case "rule":
		return &c.ruleChecker.PauseController, nil
	case "split":
		return &c.splitChecker.PauseController, nil
	case "merge":
		return &c.mergeChecker.PauseController, nil
	case "joint-state":
		return &c.jointStateChecker.PauseController, nil
	default:
		return nil, errs.ErrCheckerNotFound.FastGenByArgs()
	}
}
