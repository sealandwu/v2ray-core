// Package vmess contains the implementation of VMess protocol and transportation.
//
// VMess contains both inbound and outbound connections. VMess inbound is usually used on servers
// together with 'freedom' to talk to final destination, while VMess outbound is usually used on
// clients with 'socks' for proxying.
package vmess

//go:generate go run $GOPATH/src/v2ray.com/core/common/errors/errorgen/main.go -pkg vmess -path Proxy,VMess

import (
	"context"
	"sync"
	"time"

	"v2ray.com/core/common"
	"v2ray.com/core/common/protocol"
)

const (
	updateIntervalSec = 10
	cacheDurationSec  = 120
)

type idEntry struct {
	id      *protocol.ID
	userIdx int
	lastSec protocol.Timestamp
}

type TimedUserValidator struct {
	sync.RWMutex
	validUsers []*protocol.User
	userHash   map[[16]byte]indexTimePair
	ids        []*idEntry
	hasher     protocol.IDHash
	baseTime   protocol.Timestamp
}

type indexTimePair struct {
	index   int
	timeInc uint32
}

func NewTimedUserValidator(ctx context.Context, hasher protocol.IDHash) protocol.UserValidator {
	tus := &TimedUserValidator{
		validUsers: make([]*protocol.User, 0, 16),
		userHash:   make(map[[16]byte]indexTimePair, 512),
		ids:        make([]*idEntry, 0, 512),
		hasher:     hasher,
		baseTime:   protocol.Timestamp(time.Now().Unix() - cacheDurationSec*3),
	}
	go tus.updateUserHash(ctx, updateIntervalSec*time.Second)
	return tus
}

func (v *TimedUserValidator) generateNewHashes(nowSec protocol.Timestamp, idx int, entry *idEntry) {
	var hashValue [16]byte
	idHash := v.hasher(entry.id.Bytes())
	for entry.lastSec <= nowSec {
		common.Must2(idHash.Write(entry.lastSec.Bytes(nil)))
		idHash.Sum(hashValue[:0])
		idHash.Reset()

		v.userHash[hashValue] = indexTimePair{
			index:   idx,
			timeInc: uint32(entry.lastSec - v.baseTime),
		}

		entry.lastSec++
	}
}

func (v *TimedUserValidator) removeExpiredHashes(expire uint32) {
	for key, pair := range v.userHash {
		if pair.timeInc < expire {
			delete(v.userHash, key)
		}
	}
}

func (v *TimedUserValidator) updateUserHash(ctx context.Context, interval time.Duration) {
	for {
		select {
		case now := <-time.After(interval):
			nowSec := protocol.Timestamp(now.Unix() + cacheDurationSec)
			v.Lock()
			for _, entry := range v.ids {
				v.generateNewHashes(nowSec, entry.userIdx, entry)
			}

			expire := protocol.Timestamp(now.Unix() - cacheDurationSec*3)
			if expire > v.baseTime {
				v.removeExpiredHashes(uint32(expire - v.baseTime))
			}
			v.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

func (v *TimedUserValidator) Add(user *protocol.User) error {
	v.Lock()
	defer v.Unlock()

	idx := len(v.validUsers)
	v.validUsers = append(v.validUsers, user)
	rawAccount, err := user.GetTypedAccount()
	if err != nil {
		return err
	}
	account := rawAccount.(*InternalAccount)

	nowSec := time.Now().Unix()

	entry := &idEntry{
		id:      account.ID,
		userIdx: idx,
		lastSec: protocol.Timestamp(nowSec - cacheDurationSec),
	}
	v.generateNewHashes(protocol.Timestamp(nowSec+cacheDurationSec), idx, entry)
	v.ids = append(v.ids, entry)
	for _, alterid := range account.AlterIDs {
		entry := &idEntry{
			id:      alterid,
			userIdx: idx,
			lastSec: protocol.Timestamp(nowSec - cacheDurationSec),
		}
		v.generateNewHashes(protocol.Timestamp(nowSec+cacheDurationSec), idx, entry)
		v.ids = append(v.ids, entry)
	}

	return nil
}

func (v *TimedUserValidator) Get(userHash []byte) (*protocol.User, protocol.Timestamp, bool) {
	defer v.RUnlock()
	v.RLock()

	var fixedSizeHash [16]byte
	copy(fixedSizeHash[:], userHash)
	pair, found := v.userHash[fixedSizeHash]
	if found {
		return v.validUsers[pair.index], protocol.Timestamp(pair.timeInc) + v.baseTime, true
	}
	return nil, 0, false
}
