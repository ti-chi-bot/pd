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

package operator

import (
	"bytes"
	"fmt"

	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/log"
	"github.com/tikv/pd/server/core"
	"github.com/tikv/pd/server/schedule/storelimit"
	"go.uber.org/zap"
)

// OpStep describes the basic scheduling steps that can not be subdivided.
type OpStep interface {
	fmt.Stringer
	ConfVerChanged(region *core.RegionInfo) bool
	IsFinish(region *core.RegionInfo) bool
	CheckSafety(region *core.RegionInfo) error
	Influence(opInfluence OpInfluence, region *core.RegionInfo)
}

// TransferLeader is an OpStep that transfers a region's leader.
type TransferLeader struct {
	FromStore, ToStore uint64
}

// ConfVerChanged returns true if the conf version has been changed by this step
func (tl TransferLeader) ConfVerChanged(region *core.RegionInfo) bool {
	return false // transfer leader never change the conf version
}

func (tl TransferLeader) String() string {
	return fmt.Sprintf("transfer leader from store %v to store %v", tl.FromStore, tl.ToStore)
}

// IsFinish checks if current step is finished.
func (tl TransferLeader) IsFinish(region *core.RegionInfo) bool {
	return region.GetLeader().GetStoreId() == tl.ToStore
}

// CheckSafety checks if the step meets the safety properties.
func (tl TransferLeader) CheckSafety(region *core.RegionInfo) error {
	peer := region.GetStorePeer(tl.ToStore)
	if peer == nil {
		return errors.New("peer does not existed")
	}
	if peer.IsLearner {
		return errors.New("peer already is a learner")
	}
	return nil
}

// Influence calculates the store difference that current step makes.
func (tl TransferLeader) Influence(opInfluence OpInfluence, region *core.RegionInfo) {
	from := opInfluence.GetStoreInfluence(tl.FromStore)
	to := opInfluence.GetStoreInfluence(tl.ToStore)

	from.LeaderSize -= region.GetApproximateSize()
	from.LeaderCount--
	to.LeaderSize += region.GetApproximateSize()
	to.LeaderCount++
}

// AddPeer is an OpStep that adds a region peer.
type AddPeer struct {
	ToStore, PeerID uint64
}

// ConfVerChanged returns true if the conf version has been changed by this step
func (ap AddPeer) ConfVerChanged(region *core.RegionInfo) bool {
	if p := region.GetStoreVoter(ap.ToStore); p != nil {
		return p.GetId() == ap.PeerID
	}
	return false
}
func (ap AddPeer) String() string {
	return fmt.Sprintf("add peer %v on store %v", ap.PeerID, ap.ToStore)
}

// IsFinish checks if current step is finished.
func (ap AddPeer) IsFinish(region *core.RegionInfo) bool {
	if p := region.GetStoreVoter(ap.ToStore); p != nil {
		if p.GetId() != ap.PeerID {
			log.Warn("obtain unexpected peer", zap.String("expect", ap.String()), zap.Uint64("obtain-voter", p.GetId()))
			return false
		}
		return region.GetPendingVoter(p.GetId()) == nil
	}
	return false
}

// Influence calculates the store difference that current step makes.
func (ap AddPeer) Influence(opInfluence OpInfluence, region *core.RegionInfo) {
	to := opInfluence.GetStoreInfluence(ap.ToStore)

	regionSize := region.GetApproximateSize()
	to.RegionSize += regionSize
	to.RegionCount++
	to.AdjustStepCost(storelimit.AddPeer, regionSize)
}

// CheckSafety checks if the step meets the safety properties.
func (ap AddPeer) CheckSafety(region *core.RegionInfo) error {
	peer := region.GetStorePeer(ap.ToStore)
	if peer != nil && peer.GetId() != ap.PeerID {
		return errors.Errorf("peer %d has already existed in store %d, the operator is trying to add peer %d on the same store", peer.GetId(), ap.ToStore, ap.PeerID)
	}
	return nil
}

// AddLearner is an OpStep that adds a region learner peer.
type AddLearner struct {
	ToStore, PeerID uint64
}

// ConfVerChanged returns true if the conf version has been changed by this step
func (al AddLearner) ConfVerChanged(region *core.RegionInfo) bool {
	if p := region.GetStorePeer(al.ToStore); p != nil {
		return p.GetId() == al.PeerID
	}
	return false
}

func (al AddLearner) String() string {
	return fmt.Sprintf("add learner peer %v on store %v", al.PeerID, al.ToStore)
}

// IsFinish checks if current step is finished.
func (al AddLearner) IsFinish(region *core.RegionInfo) bool {
	if p := region.GetStoreLearner(al.ToStore); p != nil {
		if p.GetId() != al.PeerID {
			log.Warn("obtain unexpected peer", zap.String("expect", al.String()), zap.Uint64("obtain-learner", p.GetId()))
			return false
		}
		return region.GetPendingLearner(p.GetId()) == nil
	}
	return false
}

// CheckSafety checks if the step meets the safety properties.
func (al AddLearner) CheckSafety(region *core.RegionInfo) error {
	peer := region.GetStorePeer(al.ToStore)
	if peer == nil {
		return nil
	}
	if peer.GetId() != al.PeerID {
		return errors.Errorf("peer %d has already existed in store %d, the operator is trying to add peer %d on the same store", peer.GetId(), al.ToStore, al.PeerID)
	}
	if !peer.IsLearner {
		return errors.New("peer already is a voter")
	}
	return nil
}

// Influence calculates the store difference that current step makes.
func (al AddLearner) Influence(opInfluence OpInfluence, region *core.RegionInfo) {
	to := opInfluence.GetStoreInfluence(al.ToStore)

	regionSize := region.GetApproximateSize()
	to.RegionSize += regionSize
	to.RegionCount++
	to.AdjustStepCost(storelimit.AddPeer, regionSize)
}

// PromoteLearner is an OpStep that promotes a region learner peer to normal voter.
type PromoteLearner struct {
	ToStore, PeerID uint64
}

// ConfVerChanged returns true if the conf version has been changed by this step
func (pl PromoteLearner) ConfVerChanged(region *core.RegionInfo) bool {
	if p := region.GetStoreVoter(pl.ToStore); p != nil {
		return p.GetId() == pl.PeerID
	}
	return false
}

func (pl PromoteLearner) String() string {
	return fmt.Sprintf("promote learner peer %v on store %v to voter", pl.PeerID, pl.ToStore)
}

// IsFinish checks if current step is finished.
func (pl PromoteLearner) IsFinish(region *core.RegionInfo) bool {
	if p := region.GetStoreVoter(pl.ToStore); p != nil {
		if p.GetId() != pl.PeerID {
			log.Warn("obtain unexpected peer", zap.String("expect", pl.String()), zap.Uint64("obtain-voter", p.GetId()))
		}
		return p.GetId() == pl.PeerID
	}
	return false
}

// CheckSafety checks if the step meets the safety properties.
func (pl PromoteLearner) CheckSafety(region *core.RegionInfo) error {
	peer := region.GetStorePeer(pl.ToStore)
	if peer == nil {
		return errors.New("peer does not exist")
	}
	return nil
}

// Influence calculates the store difference that current step makes.
func (pl PromoteLearner) Influence(opInfluence OpInfluence, region *core.RegionInfo) {}

// RemovePeer is an OpStep that removes a region peer.
type RemovePeer struct {
<<<<<<< HEAD
	FromStore uint64
=======
	FromStore, PeerID uint64
	IsDownStore       bool
>>>>>>> 1a7caa95c (schedule: not limit remove peer of the down store (#4097))
}

// ConfVerChanged returns true if the conf version has been changed by this step
func (rp RemovePeer) ConfVerChanged(region *core.RegionInfo) bool {
	return region.GetStorePeer(rp.FromStore) == nil
}

func (rp RemovePeer) String() string {
	return fmt.Sprintf("remove peer on store %v", rp.FromStore)
}

// IsFinish checks if current step is finished.
func (rp RemovePeer) IsFinish(region *core.RegionInfo) bool {
	return region.GetStorePeer(rp.FromStore) == nil
}

// CheckSafety checks if the step meets the safety properties.
func (rp RemovePeer) CheckSafety(region *core.RegionInfo) error {
	if rp.FromStore == region.GetLeader().GetStoreId() {
		return errors.New("cannot remove leader peer")
	}
	return nil
}

// Influence calculates the store difference that current step makes.
func (rp RemovePeer) Influence(opInfluence OpInfluence, region *core.RegionInfo) {
	from := opInfluence.GetStoreInfluence(rp.FromStore)

	regionSize := region.GetApproximateSize()
	from.RegionSize -= regionSize
	from.RegionCount--
	if rp.IsDownStore {
		from.AdjustStepCost(storelimit.RemovePeer, storelimit.SmallRegionThreshold)
		return
	}
	from.AdjustStepCost(storelimit.RemovePeer, regionSize)
}

// MergeRegion is an OpStep that merge two regions.
type MergeRegion struct {
	FromRegion *metapb.Region
	ToRegion   *metapb.Region
	// there are two regions involved in merge process,
	// so to keep them from other scheduler,
	// both of them should add MerRegion operatorStep.
	// But actually, TiKV just needs the region want to be merged to get the merge request,
	// thus use a IsPassive mark to indicate that
	// this region doesn't need to send merge request to TiKV.
	IsPassive bool
}

// ConfVerChanged returns true if the conf version has been changed by this step
func (mr MergeRegion) ConfVerChanged(region *core.RegionInfo) bool {
	return false
}

func (mr MergeRegion) String() string {
	return fmt.Sprintf("merge region %v into region %v", mr.FromRegion.GetId(), mr.ToRegion.GetId())
}

// IsFinish checks if current step is finished.
func (mr MergeRegion) IsFinish(region *core.RegionInfo) bool {
	if mr.IsPassive {
		return !bytes.Equal(region.GetStartKey(), mr.ToRegion.StartKey) || !bytes.Equal(region.GetEndKey(), mr.ToRegion.EndKey)
	}
	return false
}

// CheckSafety checks if the step meets the safety properties.
func (mr MergeRegion) CheckSafety(region *core.RegionInfo) error {
	return nil
}

// Influence calculates the store difference that current step makes.
func (mr MergeRegion) Influence(opInfluence OpInfluence, region *core.RegionInfo) {
	if mr.IsPassive {
		for _, p := range region.GetPeers() {
			o := opInfluence.GetStoreInfluence(p.GetStoreId())
			o.RegionCount--
			if region.GetLeader().GetId() == p.GetId() {
				o.LeaderCount--
			}
		}
	}
}

// SplitRegion is an OpStep that splits a region.
type SplitRegion struct {
	StartKey, EndKey []byte
	Policy           pdpb.CheckPolicy
	SplitKeys        [][]byte
}

// ConfVerChanged returns true if the conf version has been changed by this step
func (sr SplitRegion) ConfVerChanged(region *core.RegionInfo) bool {
	return false
}

func (sr SplitRegion) String() string {
	return fmt.Sprintf("split region with policy %s", sr.Policy.String())
}

// IsFinish checks if current step is finished.
func (sr SplitRegion) IsFinish(region *core.RegionInfo) bool {
	return !bytes.Equal(region.GetStartKey(), sr.StartKey) || !bytes.Equal(region.GetEndKey(), sr.EndKey)
}

// Influence calculates the store difference that current step makes.
func (sr SplitRegion) Influence(opInfluence OpInfluence, region *core.RegionInfo) {
	for _, p := range region.GetPeers() {
		inf := opInfluence.GetStoreInfluence(p.GetStoreId())
		inf.RegionCount++
		if region.GetLeader().GetId() == p.GetId() {
			inf.LeaderCount++
		}
	}
}

// CheckSafety checks if the step meets the safety properties.
func (sr SplitRegion) CheckSafety(region *core.RegionInfo) error {
	return nil
}

// AddLightPeer is an OpStep that adds a region peer without considering the influence.
type AddLightPeer struct {
	ToStore, PeerID uint64
}

// ConfVerChanged returns true if the conf version has been changed by this step
func (ap AddLightPeer) ConfVerChanged(region *core.RegionInfo) bool {
	if p := region.GetStoreVoter(ap.ToStore); p != nil {
		return p.GetId() == ap.PeerID
	}
	return false
}

func (ap AddLightPeer) String() string {
	return fmt.Sprintf("add peer %v on store %v", ap.PeerID, ap.ToStore)
}

// IsFinish checks if current step is finished.
func (ap AddLightPeer) IsFinish(region *core.RegionInfo) bool {
	if p := region.GetStoreVoter(ap.ToStore); p != nil {
		if p.GetId() != ap.PeerID {
			log.Warn("obtain unexpected peer", zap.String("expect", ap.String()), zap.Uint64("obtain-voter", p.GetId()))
			return false
		}
		return region.GetPendingVoter(p.GetId()) == nil
	}
	return false
}

// CheckSafety checks if the step meets the safety properties.
func (ap AddLightPeer) CheckSafety(region *core.RegionInfo) error {
	peer := region.GetStorePeer(ap.ToStore)
	if peer != nil && peer.GetId() != ap.PeerID {
		return errors.Errorf("peer %d has already existed in store %d, the operator is trying to add peer %d on the same store", peer.GetId(), ap.ToStore, ap.PeerID)
	}
	return nil
}

// Influence calculates the store difference that current step makes.
func (ap AddLightPeer) Influence(opInfluence OpInfluence, region *core.RegionInfo) {
	to := opInfluence.GetStoreInfluence(ap.ToStore)

	to.RegionSize += region.GetApproximateSize()
	to.RegionCount++
}

// AddLightLearner is an OpStep that adds a region learner peer without considering the influence.
type AddLightLearner struct {
	ToStore, PeerID uint64
}

// ConfVerChanged returns true if the conf version has been changed by this step
func (al AddLightLearner) ConfVerChanged(region *core.RegionInfo) bool {
	if p := region.GetStorePeer(al.ToStore); p != nil {
		return p.GetId() == al.PeerID
	}
	return false
}

func (al AddLightLearner) String() string {
	return fmt.Sprintf("add learner peer %v on store %v", al.PeerID, al.ToStore)
}

// IsFinish checks if current step is finished.
func (al AddLightLearner) IsFinish(region *core.RegionInfo) bool {
	if p := region.GetStoreLearner(al.ToStore); p != nil {
		if p.GetId() != al.PeerID {
			log.Warn("obtain unexpected peer", zap.String("expect", al.String()), zap.Uint64("obtain-learner", p.GetId()))
			return false
		}
		return region.GetPendingLearner(p.GetId()) == nil
	}
	return false
}

// CheckSafety checks if the step meets the safety properties.
func (al AddLightLearner) CheckSafety(region *core.RegionInfo) error {
	peer := region.GetStorePeer(al.ToStore)
	if peer == nil {
		return nil
	}
	if peer.GetId() != al.PeerID {
		return errors.Errorf("peer %d has already existed in store %d, the operator is trying to add peer %d on the same store", peer.GetId(), al.ToStore, al.PeerID)
	}
	if !peer.IsLearner {
		return errors.New("peer already is a voter")
	}
	return nil
}

// Influence calculates the store difference that current step makes.
func (al AddLightLearner) Influence(opInfluence OpInfluence, region *core.RegionInfo) {
	to := opInfluence.GetStoreInfluence(al.ToStore)

	to.RegionSize += region.GetApproximateSize()
	to.RegionCount++
}
