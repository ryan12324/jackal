/*
 * Copyright (c) 2018 Miguel Ángel Ortuño.
 * See the LICENSE file for more information.
 */

package c2s

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ortuman/jackal/log"
	"github.com/ortuman/jackal/storage"
	"github.com/ortuman/jackal/stream"
	"github.com/ortuman/jackal/xml"
)

var (
	// ErrNotExistingAccount will be returned by Route method
	// if destination user does not exist.
	ErrNotExistingAccount = errors.New("c2s: account does not exist")

	// ErrResourceNotFound will be returned by Route method
	// if destination resource does not match any of user's available resources.
	ErrResourceNotFound = errors.New("c2s: resource not found")

	// ErrNotAuthenticated will be returned by Route method if
	// destination user is not available at this moment.
	ErrNotAuthenticated = errors.New("c2s: user not authenticated")

	// ErrBlockedJID will be returned by Route method if
	// destination JID matches any of the user's blocked JID.
	ErrBlockedJID = errors.New("c2s: destination jid is blocked")
)

// Stream represents a client-to-server XMPP stream.
type Stream interface {
	ID() string

	Context() *stream.Context

	Username() string
	Domain() string
	Resource() string

	JID() *xml.JID

	IsSecured() bool
	IsAuthenticated() bool
	IsCompressed() bool

	Presence() *xml.Presence

	SendElement(element xml.XElement)
	Disconnect(err error)
}

// Manager manages the sessions associated with an account.
type Manager struct {
	cfg        *Config
	lock       sync.RWMutex
	stms       map[string]Stream
	authedStms map[string][]Stream
	blockLists map[string][]*xml.JID
}

// singleton interface
var (
	inst        *Manager
	instMu      sync.RWMutex
	initialized uint32
)

// Initialize initializes the c2s session manager.
func Initialize(cfg *Config) {
	if atomic.CompareAndSwapUint32(&initialized, 0, 1) {
		instMu.Lock()
		defer instMu.Unlock()

		inst = &Manager{
			cfg:        cfg,
			stms:       make(map[string]Stream),
			authedStms: make(map[string][]Stream),
			blockLists: make(map[string][]*xml.JID),
		}
	}
}

// Instance returns the c2s session manager instance.
func Instance() *Manager {
	instMu.RLock()
	defer instMu.RUnlock()

	if inst == nil {
		log.Fatalf("c2s manager not initialized")
	}
	return inst
}

// Shutdown shuts down c2s manager system.
// This method should be used only for testing purposes.
func Shutdown() {
	if atomic.CompareAndSwapUint32(&initialized, 1, 0) {
		instMu.Lock()
		defer instMu.Unlock()
		inst = nil
	}
}

// DefaultLocalDomain returns default local domain.
func (m *Manager) DefaultLocalDomain() string {
	return m.cfg.Domains[0]
}

// IsLocalDomain returns true if domain is a local server domain.
func (m *Manager) IsLocalDomain(domain string) bool {
	for _, localDomain := range m.cfg.Domains {
		if localDomain == domain {
			return true
		}
	}
	return false
}

// RegisterStream registers the specified client stream.
// An error will be returned in case the stream has been previously registered.
func (m *Manager) RegisterStream(stm Stream) error {
	if !m.IsLocalDomain(stm.Domain()) {
		return fmt.Errorf("invalid domain: %s", stm.Domain())
	}
	m.lock.Lock()
	_, ok := m.stms[stm.ID()]
	if ok {
		m.lock.Unlock()
		return fmt.Errorf("stream already registered: %s", stm.ID())
	}
	m.stms[stm.ID()] = stm
	m.lock.Unlock()
	log.Infof("registered stream... (id: %s)", stm.ID())
	return nil
}

// UnregisterStream unregisters the specified client stream removing
// associated resource from the manager.
// An error will be returned in case the stream has not been previously registered.
func (m *Manager) UnregisterStream(stm Stream) error {
	m.lock.Lock()
	_, ok := m.stms[stm.ID()]
	if !ok {
		m.lock.Unlock()
		return fmt.Errorf("stream not found: %s", stm.ID())
	}
	if authedStms := m.authedStms[stm.Username()]; authedStms != nil {
		res := stm.Resource()
		for i := 0; i < len(authedStms); i++ {
			if res == authedStms[i].Resource() {
				authedStms = append(authedStms[:i], authedStms[i+1:]...)
				break
			}
		}
		if len(authedStms) > 0 {
			m.authedStms[stm.Username()] = authedStms
		} else {
			delete(m.authedStms, stm.Username())
		}
	}
	delete(m.stms, stm.ID())
	m.lock.Unlock()
	log.Infof("unregistered stream... (id: %s)", stm.ID())
	return nil
}

// AuthenticateStream sets a previously registered stream as authenticated.
// An error will be returned in case no assigned resource is found.
func (m *Manager) AuthenticateStream(stm Stream) error {
	if len(stm.Resource()) == 0 {
		return fmt.Errorf("resource not yet assigned: %s", stm.ID())
	}
	m.lock.Lock()
	if authedStrms := m.authedStms[stm.Username()]; authedStrms != nil {
		m.authedStms[stm.Username()] = append(authedStrms, stm)
	} else {
		m.authedStms[stm.Username()] = []Stream{stm}
	}
	m.lock.Unlock()
	log.Infof("authenticated stream... (%s/%s)", stm.Username(), stm.Resource())
	return nil
}

// IsBlockedJID returns whether or not the passed jid matches any
// of a user's blocking list JID.
func (m *Manager) IsBlockedJID(jid *xml.JID, username string) bool {
	bl := m.getBlockList(username)
	for _, blkJID := range bl {
		if m.jidMatchesBlockedJID(jid, blkJID) {
			return true
		}
	}
	return false
}

// ReloadBlockList reloads in-memory block list for a given user and starts
// applying it for future stanza routing.
func (m *Manager) ReloadBlockList(username string) {
	m.lock.Lock()
	delete(m.blockLists, username)
	m.lock.Unlock()
	log.Infof("block list reloaded... (username: %s)", username)
}

// Route routes a stanza applying server rules for handling XML stanzas.
// (https://xmpp.org/rfcs/rfc3921.html#rules)
func (m *Manager) Route(elem xml.Stanza) error {
	return m.route(elem, false)
}

// MustRoute routes a stanza applying server rules for handling XML stanzas
// and ignoring blocking lists.
func (m *Manager) MustRoute(elem xml.Stanza) error {
	return m.route(elem, true)
}

// StreamsMatchingJID returns all available streams that match a given JID.
func (m *Manager) StreamsMatchingJID(jid *xml.JID) []Stream {
	if !m.IsLocalDomain(jid.Domain()) {
		return nil
	}
	var ret []Stream
	opts := xml.JIDMatchesDomain
	if jid.IsFull() {
		opts |= xml.JIDMatchesResource
	}

	m.lock.RLock()
	if len(jid.Node()) > 0 {
		opts |= xml.JIDMatchesNode
		stms := m.authedStms[jid.Node()]
		for _, stm := range stms {
			if stm.JID().Matches(jid, opts) {
				ret = append(ret, stm)
			}
		}
	} else {
		for _, stms := range m.authedStms {
			for _, stm := range stms {
				if stm.JID().Matches(jid, opts) {
					ret = append(ret, stm)
				}
			}
		}
	}
	m.lock.RUnlock()
	return ret
}

func (m *Manager) route(elem xml.Stanza, ignoreBlocking bool) error {
	toJID := elem.ToJID()
	if !m.IsLocalDomain(toJID.Domain()) {
		return nil
	}
	if !ignoreBlocking && !toJID.IsServer() {
		if m.IsBlockedJID(elem.FromJID(), toJID.Node()) {
			return ErrBlockedJID
		}
	}
	rcps := m.StreamsMatchingJID(toJID.ToBareJID())
	if len(rcps) == 0 {
		exists, err := storage.Instance().UserExists(toJID.Node())
		if err != nil {
			return err
		}
		if exists {
			return ErrNotAuthenticated
		}
		return ErrNotExistingAccount
	}
	if toJID.IsFullWithUser() {
		for _, stm := range rcps {
			if stm.Resource() == toJID.Resource() {
				stm.SendElement(elem)
				return nil
			}
		}
		return ErrResourceNotFound
	}
	switch elem.(type) {
	case *xml.Message:
		// send toJID highest priority stream
		stm := rcps[0]
		var highestPriority int8
		if p := stm.Presence(); p != nil {
			highestPriority = p.Priority()
		}
		for i := 1; i < len(rcps); i++ {
			rcp := rcps[i]
			if p := rcp.Presence(); p != nil && p.Priority() > highestPriority {
				stm = rcp
				highestPriority = p.Priority()
			}
		}
		stm.SendElement(elem)

	default:
		// broadcast toJID all streams
		for _, stm := range rcps {
			stm.SendElement(elem)
		}
	}
	return nil
}

func (m *Manager) getBlockList(username string) []*xml.JID {
	m.lock.RLock()
	bl := m.blockLists[username]
	m.lock.RUnlock()
	if bl != nil {
		return bl
	}
	blItms, err := storage.Instance().FetchBlockListItems(username)
	if err != nil {
		log.Error(err)
		return nil
	}
	bl = []*xml.JID{}
	for _, blItm := range blItms {
		j, _ := xml.NewJIDString(blItm.JID, true)
		bl = append(bl, j)
	}
	m.lock.Lock()
	m.blockLists[username] = bl
	m.lock.Unlock()
	return bl
}

func (m *Manager) jidMatchesBlockedJID(jid, blockedJID *xml.JID) bool {
	if blockedJID.IsFullWithUser() {
		return jid.Matches(blockedJID, xml.JIDMatchesNode|xml.JIDMatchesDomain|xml.JIDMatchesResource)
	} else if blockedJID.IsFullWithServer() {
		return jid.Matches(blockedJID, xml.JIDMatchesDomain|xml.JIDMatchesResource)
	} else if blockedJID.IsBare() {
		return jid.Matches(blockedJID, xml.JIDMatchesNode|xml.JIDMatchesDomain)
	}
	return jid.Matches(blockedJID, xml.JIDMatchesDomain)
}
