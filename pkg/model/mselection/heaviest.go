package mselection

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/willf/bitset"

	"github.com/gohornet/hornet/pkg/model/hornet"
	"github.com/gohornet/hornet/pkg/model/tangle"
	"github.com/gohornet/hornet/pkg/utils"
)

var (
	// ErrNoTipsAvailable is returned when no tips are available in the node.
	ErrNoTipsAvailable = errors.New("no tips available")
)

// HeaviestSelector implements the heaviest branch selection strategy.
type HeaviestSelector struct {
	sync.Mutex

	minHeaviestBranchUnreferencedMessagesThreshold int
	maxHeaviestBranchTipsPerCheckpoint             int
	randomTipsPerCheckpoint                        int
	heaviestBranchSelectionDeadline                time.Duration

	trackedMessages map[string]*trackedMessage // map of all tracked messages
	tips            *list.List                 // list of available tips
}

type trackedMessage struct {
	messageID *hornet.MessageID // message ID of the corresponding message
	tip       *list.Element     // pointer to the element in the tip list
	refs      *bitset.BitSet    // BitSet of all the referenced messages
}

type trackedMessagesList struct {
	msgs map[string]*trackedMessage
}

// Len returns the length of the inner msgs slice.
func (il *trackedMessagesList) Len() int {
	return len(il.msgs)
}

// randomTip selects a random tip item from the trackedMessagesList.
func (il *trackedMessagesList) randomTip() (*trackedMessage, error) {
	if len(il.msgs) == 0 {
		return nil, ErrNoTipsAvailable
	}

	randomMsgIndex := utils.RandomInsecure(0, len(il.msgs)-1)

	for _, tip := range il.msgs {
		randomMsgIndex--

		// if randomMsgIndex is below zero, we return the given tip
		if randomMsgIndex < 0 {
			return tip, nil
		}
	}

	return nil, ErrNoTipsAvailable
}

// referenceTip removes the tip and set all bits of all referenced
// messages of the tip in all existing tips to zero.
// this way we can track which parts of the cone would already be referenced by this tip, and
// correctly calculate the weight of the remaining tips.
func (il *trackedMessagesList) referenceTip(tip *trackedMessage) {

	il.removeTip(tip)

	// set all bits of all referenced messages in all existing tips to zero
	for _, otherTip := range il.msgs {
		otherTip.refs.InPlaceDifference(tip.refs)
	}
}

// removeTip removes the tip from the map.
func (il *trackedMessagesList) removeTip(tip *trackedMessage) {
	delete(il.msgs, tip.messageID.MapKey())
}

// New creates a new HeaviestSelector instance.
func New(minHeaviestBranchUnreferencedMessagesThreshold int, maxHeaviestBranchTipsPerCheckpoint int, randomTipsPerCheckpoint int, heaviestBranchSelectionDeadline time.Duration) *HeaviestSelector {
	s := &HeaviestSelector{
		minHeaviestBranchUnreferencedMessagesThreshold: minHeaviestBranchUnreferencedMessagesThreshold,
		maxHeaviestBranchTipsPerCheckpoint:             maxHeaviestBranchTipsPerCheckpoint,
		randomTipsPerCheckpoint:                        randomTipsPerCheckpoint,
		heaviestBranchSelectionDeadline:                heaviestBranchSelectionDeadline,
	}
	s.reset()
	return s
}

// reset resets the tracked messages map and tips list of s.
func (s *HeaviestSelector) reset() {
	s.Lock()
	defer s.Unlock()

	// create an empty map
	s.trackedMessages = make(map[string]*trackedMessage)

	// create an empty list
	s.tips = list.New()
}

// selectTip selects a tip to be used for the next checkpoint.
// it returns a tip, confirming the most messages in the future cone,
// and the amount of referenced messages of this tip, that were not referenced by previously chosen tips.
func (s *HeaviestSelector) selectTip(tipsList *trackedMessagesList) (*trackedMessage, uint, error) {

	if tipsList.Len() == 0 {
		return nil, 0, ErrNoTipsAvailable
	}

	var best = struct {
		tips  []*trackedMessage
		count uint
	}{
		tips:  []*trackedMessage{},
		count: 0,
	}

	// loop through all tips and find the one with the most referenced messages
	for _, tip := range tipsList.msgs {
		c := tip.refs.Count()
		if c > best.count {
			// tip with heavier branch found
			best.tips = []*trackedMessage{
				tip,
			}
			best.count = c
		} else if c == best.count {
			// add the tip to the slice of currently best tips
			best.tips = append(best.tips, tip)
		}
	}

	if len(best.tips) == 0 {
		return nil, 0, ErrNoTipsAvailable
	}

	// select a random tip from the provided slice of tips.
	selected := best.tips[utils.RandomInsecure(0, len(best.tips)-1)]

	return selected, best.count, nil
}

// SelectTips tries to collect tips that confirm the most recent messages since the last reset of the selector.
// best tips are determined by counting the referenced messages (heaviest branches) and by "removing" the
// messages of the referenced cone of the already chosen tips in the bitsets of the available tips.
// only tips are considered that were present at the beginning of the SelectTips call,
// to prevent attackers from creating heavier branches while we are searching the best tips.
// "maxHeaviestBranchTipsPerCheckpoint" is the amount of tips that are collected if
// the current best tip is not below "UnreferencedMessagesThreshold" before.
// a minimum amount of selected tips can be enforced, even if none of the heaviest branches matches the
// "minHeaviestBranchUnreferencedMessagesThreshold" criteria.
// if at least one heaviest branch tip was found, "randomTipsPerCheckpoint" random tips are added
// to add some additional randomness to prevent parasite chain attacks.
// the selection is canceled after a fixed deadline. in this case, it returns the current collected tips.
func (s *HeaviestSelector) SelectTips(minRequiredTips int) (hornet.MessageIDs, error) {

	// create a working list with the current tips to release the lock to allow faster iteration
	// and to get a frozen view of the tangle, so an attacker can't
	// create heavier branches while we are searching the best tips
	// caution: the tips are not copied, do not mutate!
	tipsList := s.tipsToList()

	// tips could be empty after a reset
	if tipsList.Len() == 0 {
		return nil, ErrNoTipsAvailable
	}

	var tips hornet.MessageIDs

	// run the tip selection for at most 0.1s to keep the view on the tangle recent; this should be plenty
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(s.heaviestBranchSelectionDeadline))
	defer cancel()

	deadlineExceeded := false

	for i := 0; i < s.maxHeaviestBranchTipsPerCheckpoint; i++ {
		// when the context has been canceled, stop collecting heaviest branch tips
		select {
		case <-ctx.Done():
			deadlineExceeded = true
		default:
		}

		tip, count, err := s.selectTip(tipsList)
		if err != nil {
			break
		}

		if (len(tips) > minRequiredTips) && ((count < uint(s.minHeaviestBranchUnreferencedMessagesThreshold)) || deadlineExceeded) {
			// minimum amount of tips reached and the heaviest tips do not confirm enough messages or the deadline was exceeded
			// => no need to collect more
			break
		}

		tipsList.referenceTip(tip)
		tips = append(tips, tip.messageID)
	}

	if len(tips) == 0 {
		return nil, ErrNoTipsAvailable
	}

	// also pick random tips if at least one heaviest branch tip was found
	for i := 0; i < s.randomTipsPerCheckpoint; i++ {
		item, err := tipsList.randomTip()
		if err != nil {
			break
		}

		tipsList.referenceTip(item)
		tips = append(tips, item.messageID)
	}

	// reset the whole HeaviestSelector if valid tips were found
	s.reset()

	return tips, nil
}

// OnNewSolidMessage adds a new message to be processed by s.
// The message must be solid and OnNewSolidMessage must be called in the order of solidification.
// The message must also not be below max depth.
func (s *HeaviestSelector) OnNewSolidMessage(msgMeta *tangle.MessageMetadata) (trackedMessagesCount int) {
	s.Lock()
	defer s.Unlock()

	// filter duplicate messages
	if _, contains := s.trackedMessages[msgMeta.GetMessageID().MapKey()]; contains {
		return
	}

	parent1Item := s.trackedMessages[msgMeta.GetParent1MessageID().MapKey()]
	parent2Item := s.trackedMessages[msgMeta.GetParent2MessageID().MapKey()]

	// compute the referenced messages
	// all the known children in the HeaviestSelector are represented by a unique bit in a bitset.
	// if a new child is added, we expand the bitset by 1 bit and store the Union of the bitsets
	// of parent1 and parent2 for this child, to know which parts of the cone are referenced by this child.
	idx := uint(len(s.trackedMessages))
	it := &trackedMessage{messageID: msgMeta.GetMessageID(), refs: bitset.New(idx + 1).Set(idx)}
	if parent1Item != nil {
		it.refs.InPlaceUnion(parent1Item.refs)
	}
	if parent2Item != nil {
		it.refs.InPlaceUnion(parent2Item.refs)
	}
	s.trackedMessages[it.messageID.MapKey()] = it

	// update tips
	s.removeTip(parent1Item)
	s.removeTip(parent2Item)
	it.tip = s.tips.PushBack(it)

	return s.GetTrackedMessagesCount()
}

// removeTip removes the tip item from s.
func (s *HeaviestSelector) removeTip(it *trackedMessage) {
	if it == nil || it.tip == nil {
		return
	}
	s.tips.Remove(it.tip)
	it.tip = nil
}

// tipsToList returns a new list containing the current tips.
func (s *HeaviestSelector) tipsToList() *trackedMessagesList {
	s.Lock()
	defer s.Unlock()

	result := make(map[string]*trackedMessage)
	for e := s.tips.Front(); e != nil; e = e.Next() {
		tip := e.Value.(*trackedMessage)
		result[tip.messageID.MapKey()] = tip
	}
	return &trackedMessagesList{msgs: result}
}

// GetTrackedMessagesCount returns the amount of known messages.
func (s *HeaviestSelector) GetTrackedMessagesCount() (trackedMessagesCount int) {
	return len(s.trackedMessages)
}
