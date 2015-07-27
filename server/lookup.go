// Copyright 2014-2015 The Dename Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License. You may obtain a copy of
// the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations under
// the License.

package server

import (
	"encoding/binary"
	"fmt"
	"log"

	"github.com/yahoo/coname/common"
	"github.com/yahoo/coname/common/vrf"
	"github.com/yahoo/coname/proto"
	"github.com/yahoo/coname/server/kv"
	"golang.org/x/net/context"
)

const lookupMaxChainLength = 100

// LookupProfile implements proto.E2EKSLookupServer
func (ks *Keyserver) LookupProfile(ctx context.Context, req *proto.LookupProfileRequest) (*proto.LookupProof, error) {
	ret := &proto.LookupProof{UserId: req.UserId}
	index := req.Index
	if index == nil {
		index = vrf.Compute([]byte(req.UserId), ks.vrfSecret)
		ret.IndexProof = vrf.Prove([]byte(req.UserId), ks.vrfSecret)
	}
	remainingVerifiers := common.ListQuorum(req.QuorumRequirement, nil)
	haveVerifiers := make(map[uint64]struct{}, len(remainingVerifiers))
	// find latest epoch, iterate backwards until quorum requirement is met
	oldestEpoch, newestEpoch := uint64(1), ks.lastRatifiedEpoch()
	// 0 is bad for iterating uint64 in the negative direction and there is no epoch 0
	if newestEpoch-oldestEpoch > lookupMaxChainLength {
		oldestEpoch = newestEpoch - lookupMaxChainLength
	}
	lookupEpoch := newestEpoch
	for epoch := newestEpoch; epoch >= oldestEpoch &&
		!common.CheckQuorum(req.QuorumRequirement, haveVerifiers) &&
		len(remainingVerifiers) != 0; epoch-- {
		for verifier := range remainingVerifiers {
			ratificationBytes, err := ks.db.Get(tableRatifications(epoch, verifier))
			switch err {
			case nil:
			case ks.db.ErrNotFound():
				continue
			default:
				log.Printf("ERROR: ks.db.Get(tableRatifications(%d, %d): %s", epoch, verifier, err)
				return nil, fmt.Errorf("internal error")
			}
			ratification := new(proto.SignedRatification)
			if err := ratification.Unmarshal(ratificationBytes); err != nil {
				log.Printf("ERROR: invalid protobuf in ratifications db (epoch %d, verifier %d): %s", epoch, verifier, err)
				return nil, fmt.Errorf("internal error")
			}
			ret.Ratifications = append(ret.Ratifications, ratification)
			lookupEpoch = epoch
			delete(remainingVerifiers, verifier)
			haveVerifiers[verifier] = struct{}{}
		}
	}
	// reverse the order
	for i := 0; i < len(ret.Ratifications)/2; i++ {
		a, b := ret.Ratifications[i], ret.Ratifications[len(ret.Ratifications)-1-i]
		ret.Ratifications[i], ret.Ratifications[len(ret.Ratifications)-1-i] = b, a
	}
	// TODO(dmz): ret.TreeProof = ks.merkletreeForEpoch(lookupEpoch).Lookup(index)
	seu, err := ks.getUpdate(index, lookupEpoch)
	if err != nil {
		log.Printf("ERROR: getProfile of %x at or before epoch %d: %s", index, lookupEpoch, err)
		return nil, fmt.Errorf("internal error")
	}
	if seu != nil {
		ret.Profile = seu.Profile
	}
	if !common.CheckQuorum(req.QuorumRequirement, haveVerifiers) {
		return ret, fmt.Errorf("could not find sufficient verification in the last %d epochs (and not bothering to look further into the past)", lookupMaxChainLength)
	}
	return ret, nil
}

// lastRatifiedEpoch returns the last epoch for which we have a ratification.
func (ks *Keyserver) lastRatifiedEpoch() uint64 {
	iter := ks.db.NewIterator(kv.BytesPrefix([]byte{tableRatificationsPrefix}))
	if !iter.Last() {
		return 0
	}
	ret := binary.BigEndian.Uint64(iter.Key()[1 : 1+8])
	iter.Release()
	if iter.Error() != nil {
		log.Printf("ERROR: db scan for last ratification: %s", iter.Error())
		return 0
	}
	return ret
}

// getUpdate returns the last update to profile of idx during or before epoch.
// If there is no such update, (nil, nil) is returned.
func (ks *Keyserver) getUpdate(idx []byte, epoch uint64) (*proto.SignedEntryUpdate, error) {
	// idx: []&const
	prefixIdxEpoch := make([]byte, 1+32+8) // TODO(VRF): 32 = VRF_SIZE
	prefixIdxEpoch[0] = tableSignedUpdatesPrefix
	copy(prefixIdxEpoch[1:], idx)
	binary.BigEndian.PutUint64(prefixIdxEpoch[1+len(idx):], epoch+1)
	iter := ks.db.NewIterator(&kv.Range{
		Start: prefixIdxEpoch[:1+len(idx)],
		Limit: prefixIdxEpoch,
	})
	if !iter.Last() {
		if iter.Error() != nil {
			return nil, iter.Error()
		}
		return nil, nil
	}
	ret := new(proto.SignedEntryUpdate)
	if err := ret.Unmarshal(iter.Value()); err != nil {
		return nil, iter.Error()
	}
	iter.Release()
	return ret, nil
}
