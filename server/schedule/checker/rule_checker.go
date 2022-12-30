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
// See the License for the specific language governing permissions and
// limitations under the License.

package checker

import (
	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/log"
	"github.com/tikv/pd/pkg/cache"
	"github.com/tikv/pd/pkg/errs"
	"github.com/tikv/pd/server/core"
	"github.com/tikv/pd/server/schedule/filter"
	"github.com/tikv/pd/server/schedule/operator"
	"github.com/tikv/pd/server/schedule/opt"
	"github.com/tikv/pd/server/schedule/placement"
	"go.uber.org/zap"
)

// RuleChecker fix/improve region by placement rules.
type RuleChecker struct {
	cluster           opt.Cluster
	ruleManager       *placement.RuleManager
	name              string
	regionWaitingList cache.Cache
}

// NewRuleChecker creates a checker instance.
func NewRuleChecker(cluster opt.Cluster, ruleManager *placement.RuleManager, regionWaitingList cache.Cache) *RuleChecker {
	return &RuleChecker{
		cluster:           cluster,
		ruleManager:       ruleManager,
		name:              "rule-checker",
		regionWaitingList: regionWaitingList,
	}
}

// GetType returns RuleChecker's Type
func (c *RuleChecker) GetType() string {
	return "rule-checker"
}

// Check checks if the region matches placement rules and returns Operator to
// fix it.
func (c *RuleChecker) Check(region *core.RegionInfo) *operator.Operator {
	checkerCounter.WithLabelValues("rule_checker", "check").Inc()

	fit := c.cluster.FitRegion(region)
	if len(fit.RuleFits) == 0 {
		checkerCounter.WithLabelValues("rule_checker", "fix-range").Inc()
		// If the region matches no rules, the most possible reason is it spans across
		// multiple rules.
		return c.fixRange(region)
	}
	op, err := c.fixOrphanPeers(region, fit)
	if err == nil && op != nil {
		return op
	}
	log.Debug("fail to fix orphan peer", errs.ZapError(err))
	for _, rf := range fit.RuleFits {
		op, err := c.fixRulePeer(region, fit, rf)
		if err != nil {
			log.Debug("fail to fix rule peer", zap.String("rule-group", rf.Rule.GroupID), zap.String("rule-id", rf.Rule.ID), errs.ZapError(err))
			continue
		}
		if op != nil {
			return op
		}
	}
	return nil
}

func (c *RuleChecker) fixRange(region *core.RegionInfo) *operator.Operator {
	keys := c.ruleManager.GetSplitKeys(region.GetStartKey(), region.GetEndKey())
	if len(keys) == 0 {
		return nil
	}

	op, err := operator.CreateSplitRegionOperator("rule-split-region", region, 0, pdpb.CheckPolicy_USEKEY, keys)
	if err != nil {
		log.Debug("create split region operator failed", errs.ZapError(err))
		return nil
	}

	return op
}

func (c *RuleChecker) fixRulePeer(region *core.RegionInfo, fit *placement.RegionFit, rf *placement.RuleFit) (*operator.Operator, error) {
	// make up peers.
	if len(rf.Peers) < rf.Rule.Count {
		return c.addRulePeer(region, rf)
	}
	// fix down/offline peers.
	for _, peer := range rf.Peers {
		if c.isDownPeer(region, peer) {
			checkerCounter.WithLabelValues("rule_checker", "replace-down").Inc()
			return c.replaceRulePeer(region, rf, peer, downStatus)
		}
		if c.isOfflinePeer(region, peer) {
			checkerCounter.WithLabelValues("rule_checker", "replace-offline").Inc()
			return c.replaceRulePeer(region, rf, peer, offlineStatus)
		}
	}
	// fix loose matched peers.
	for _, peer := range rf.PeersWithDifferentRole {
		op, err := c.fixLooseMatchPeer(region, fit, rf, peer)
		if err != nil {
			return nil, err
		}
		if op != nil {
			return op, nil
		}
	}
	return c.fixBetterLocation(region, rf)
}

func (c *RuleChecker) addRulePeer(region *core.RegionInfo, rf *placement.RuleFit) (*operator.Operator, error) {
	checkerCounter.WithLabelValues("rule_checker", "add-rule-peer").Inc()
	ruleStores := c.getRuleFitStores(rf)
	store, filterByTempState := c.strategy(region, rf.Rule).SelectStoreToAdd(ruleStores)
	if store == 0 {
		checkerCounter.WithLabelValues("rule_checker", "no-store-add").Inc()
		if filterByTempState {
			c.regionWaitingList.Put(region.GetID(), nil)
		}
		return nil, errors.New("no store to add peer")
	}
	peer := &metapb.Peer{StoreId: store, Role: rf.Rule.Role.MetaPeerRole()}
	op, err := operator.CreateAddPeerOperator("add-rule-peer", c.cluster, region, peer, operator.OpReplica)
	if err != nil {
		return nil, err
	}
	op.SetPriorityLevel(core.HighPriority)
	return op, nil
}

func (c *RuleChecker) replaceRulePeer(region *core.RegionInfo, rf *placement.RuleFit, peer *metapb.Peer, status string) (*operator.Operator, error) {
	ruleStores := c.getRuleFitStores(rf)
	store, filterByTempState := c.strategy(region, rf.Rule).SelectStoreToFix(ruleStores, peer.GetStoreId())
	if store == 0 {
		checkerCounter.WithLabelValues("rule_checker", "no-store-replace").Inc()
		if filterByTempState {
			c.regionWaitingList.Put(region.GetID(), nil)
		}
		return nil, errors.New("no store to replace peer")
	}
	newPeer := &metapb.Peer{StoreId: store, Role: rf.Rule.Role.MetaPeerRole()}
	op, err := operator.CreateMovePeerOperator("replace-rule-"+status+"-peer", c.cluster, region, operator.OpReplica, peer.StoreId, newPeer)
	if err != nil {
		return nil, err
	}
	op.SetPriorityLevel(core.HighPriority)
	return op, nil
}

func (c *RuleChecker) fixLooseMatchPeer(region *core.RegionInfo, fit *placement.RegionFit, rf *placement.RuleFit, peer *metapb.Peer) (*operator.Operator, error) {
	if core.IsLearner(peer) && rf.Rule.Role != placement.Learner {
		checkerCounter.WithLabelValues("rule_checker", "fix-peer-role").Inc()
		return operator.CreatePromoteLearnerOperator("fix-peer-role", c.cluster, region, peer)
	}
	if region.GetLeader().GetId() != peer.GetId() && rf.Rule.Role == placement.Leader {
		checkerCounter.WithLabelValues("rule_checker", "fix-leader-role").Inc()
		if c.allowLeader(fit, peer) {
			return operator.CreateTransferLeaderOperator("fix-leader-role", c.cluster, region, region.GetLeader().StoreId, peer.GetStoreId(), 0)
		}
		checkerCounter.WithLabelValues("rule_checker", "not-allow-leader")
		return nil, errors.New("peer cannot be leader")
	}
	if region.GetLeader().GetId() == peer.GetId() && rf.Rule.Role == placement.Follower {
		checkerCounter.WithLabelValues("rule_checker", "fix-follower-role").Inc()
		for _, p := range region.GetPeers() {
			if c.allowLeader(fit, p) {
				return operator.CreateTransferLeaderOperator("fix-follower-role", c.cluster, region, peer.GetStoreId(), p.GetStoreId(), 0)
			}
		}
		checkerCounter.WithLabelValues("rule_checker", "no-new-leader").Inc()
		return nil, errors.New("no new leader")
	}
	return nil, nil
}

func (c *RuleChecker) allowLeader(fit *placement.RegionFit, peer *metapb.Peer) bool {
	if core.IsLearner(peer) {
		return false
	}
	s := c.cluster.GetStore(peer.GetStoreId())
	if s == nil {
		return false
	}
	stateFilter := &filter.StoreStateFilter{ActionScope: "rule-checker", TransferLeader: true}
	if !stateFilter.Target(c.cluster.GetOpts(), s) {
		return false
	}
	for _, rf := range fit.RuleFits {
		if (rf.Rule.Role == placement.Leader || rf.Rule.Role == placement.Voter) &&
			placement.MatchLabelConstraints(s, rf.Rule.LabelConstraints) {
			return true
		}
	}
	return false
}

func (c *RuleChecker) fixBetterLocation(region *core.RegionInfo, rf *placement.RuleFit) (*operator.Operator, error) {
	if len(rf.Rule.LocationLabels) == 0 || rf.Rule.Count <= 1 {
		return nil, nil
	}

	strategy := c.strategy(region, rf.Rule)
	ruleStores := c.getRuleFitStores(rf)
	oldStore := strategy.SelectStoreToRemove(ruleStores)
	if oldStore == 0 {
		return nil, nil
	}
	newStore, _ := strategy.SelectStoreToImprove(ruleStores, oldStore)
	if newStore == 0 {
		log.Debug("no replacement store", zap.Uint64("region-id", region.GetID()))
		return nil, nil
	}
	checkerCounter.WithLabelValues("rule_checker", "move-to-better-location").Inc()
	newPeer := &metapb.Peer{StoreId: newStore, Role: rf.Rule.Role.MetaPeerRole()}
	return operator.CreateMovePeerOperator("move-to-better-location", c.cluster, region, operator.OpReplica, oldStore, newPeer)
}

func (c *RuleChecker) fixOrphanPeers(region *core.RegionInfo, fit *placement.RegionFit) (*operator.Operator, error) {
	if len(fit.OrphanPeers) == 0 {
		return nil, nil
	}
	isUnhealthyPeer := func(id uint64) bool {
		for _, pendingPeer := range region.GetPendingPeers() {
			if pendingPeer.GetId() == id {
				return true
			}
		}
		for _, downPeer := range region.GetDownPeers() {
			if downPeer.Peer.GetId() == id {
				return true
			}
		}
		return false
	}
	// remove orphan peers only when all rules are satisfied (count+role) and all peers selected
	// by RuleFits is not pending or down.
	hasUnhealthyFit := false
loopFits:
	for _, rf := range fit.RuleFits {
		if !rf.IsSatisfied() {
			hasUnhealthyFit = true
			break
		}
		for _, p := range rf.Peers {
			if isUnhealthyPeer(p.GetId()) {
				hasUnhealthyFit = true
				break loopFits
			}
		}
	}
	// If hasUnhealthyFit is false, it is safe to delete the OrphanPeer.
	if !hasUnhealthyFit {
		checkerCounter.WithLabelValues("rule_checker", "remove-orphan-peer").Inc()
		return operator.CreateRemovePeerOperator("remove-orphan-peer", c.cluster, 0, region, fit.OrphanPeers[0].StoreId)
	}
	// If hasUnhealthyFit is true, try to remove unhealthy orphan peers only if number of OrphanPeers is >= 2.
	// Ref https://github.com/tikv/pd/issues/4045
	if len(fit.OrphanPeers) >= 2 {
		for _, orphanPeer := range fit.OrphanPeers {
			if isUnhealthyPeer(orphanPeer.GetId()) {
				checkerCounter.WithLabelValues("rule_checker", "remove-orphan-peer").Inc()
				return operator.CreateRemovePeerOperator("remove-orphan-peer", c.cluster, 0, region, orphanPeer.StoreId)
			}
		}
	}
	checkerCounter.WithLabelValues("rule_checker", "skip-remove-orphan-peer").Inc()
	return nil, nil
}

func (c *RuleChecker) isDownPeer(region *core.RegionInfo, peer *metapb.Peer) bool {
	for _, stats := range region.GetDownPeers() {
		if stats.GetPeer().GetId() != peer.GetId() {
			continue
		}
		storeID := peer.GetStoreId()
		store := c.cluster.GetStore(storeID)
		if store == nil {
			log.Warn("lost the store, maybe you are recovering the PD cluster", zap.Uint64("store-id", storeID))
			return false
		}
		// Only consider the state of the Store, not `stats.DownSeconds`.
		if store.DownTime() < c.cluster.GetOpts().GetMaxStoreDownTime() {
			continue
		}
		return true
	}
	return false
}

func (c *RuleChecker) isOfflinePeer(region *core.RegionInfo, peer *metapb.Peer) bool {
	store := c.cluster.GetStore(peer.GetStoreId())
	if store == nil {
		log.Warn("lost the store, maybe you are recovering the PD cluster", zap.Uint64("store-id", peer.StoreId))
		return false
	}
	return !store.IsUp()
}

func (c *RuleChecker) strategy(region *core.RegionInfo, rule *placement.Rule) *ReplicaStrategy {
	return &ReplicaStrategy{
		checkerName:    c.name,
		cluster:        c.cluster,
		isolationLevel: rule.IsolationLevel,
		locationLabels: rule.LocationLabels,
		region:         region,
		extraFilters:   []filter.Filter{filter.NewLabelConstaintFilter(c.name, rule.LabelConstraints)},
	}
}

func (c *RuleChecker) getRuleFitStores(rf *placement.RuleFit) []*core.StoreInfo {
	var stores []*core.StoreInfo
	for _, p := range rf.Peers {
		if s := c.cluster.GetStore(p.GetStoreId()); s != nil {
			stores = append(stores, s)
		}
	}
	return stores
}
