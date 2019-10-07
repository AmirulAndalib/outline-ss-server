// Copyright 2018 Jigsaw Operations LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package shadowsocks

import (
	"container/list"
	"net"
	"sync"

	"github.com/shadowsocks/go-shadowsocks2/shadowaead"
)

// CipherEntry holds a Cipher with an identifier.
// The public fields are constant, but lastAddress is mutable under cipherList.mu.
type CipherEntry struct {
	ID           string
	Cipher       shadowaead.Cipher
	lastClientIP net.IP
}

// CipherList is a list of CipherEntry elements that allows for thread-safe snapshotting and
// moving to front.
type CipherList interface {
	PushBack(id string, cipher shadowaead.Cipher) *list.Element
	SafeSnapshotForClientIP(clientIP net.IP) []*list.Element
	SafeMarkUsedByClientIP(e *list.Element, clientIP net.IP)
}

type cipherList struct {
	CipherList
	list *list.List
	mu   sync.RWMutex
}

// NewCipherList creates an empty CipherList
func NewCipherList() CipherList {
	return &cipherList{list: list.New()}
}

func (cl *cipherList) PushBack(id string, cipher shadowaead.Cipher) *list.Element {
	return cl.list.PushBack(&CipherEntry{ID: id, Cipher: cipher})
}

func matchesIP(e *list.Element, clientIP net.IP) bool {
	c := e.Value.(*CipherEntry)
	return clientIP != nil && clientIP.Equal(c.lastClientIP)
}

func (cl *cipherList) SafeSnapshotForClientIP(clientIP net.IP) []*list.Element {
	cl.mu.RLock()
	defer cl.mu.RUnlock()
	cipherArray := make([]*list.Element, 0, cl.list.Len())
	remainingCiphers := make([]*list.Element, 0, cl.list.Len())
	// Put all ciphers with matching last known IP at the front.
	for e := cl.list.Front(); e != nil; e = e.Next() {
		if matchesIP(e, clientIP) {
			cipherArray = append(cipherArray, e)
		} else {
			remainingCiphers = append(remainingCiphers, e)
		}
	}
	// Include all remaining ciphers in recency order.
	return append(cipherArray, remainingCiphers...)
}

func (cl *cipherList) SafeMarkUsedByClientIP(e *list.Element, clientIP net.IP) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.list.MoveToFront(e)

	c := e.Value.(*CipherEntry)
	c.lastClientIP = clientIP
}
