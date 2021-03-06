/*
 * Copyright (c) 2018 Miguel Ángel Ortuño.
 * See the LICENSE file for more information.
 */

package xep0077

import (
	"github.com/ortuman/jackal/log"
	"github.com/ortuman/jackal/storage"
	"github.com/ortuman/jackal/storage/model"
	"github.com/ortuman/jackal/stream/c2s"
	"github.com/ortuman/jackal/xml"
)

const registerNamespace = "jabber:iq:register"

// Config represents XMPP In-Band Registration module (XEP-0077) configuration.
type Config struct {
	AllowRegistration bool `yaml:"allow_registration"`
	AllowChange       bool `yaml:"allow_change"`
	AllowCancel       bool `yaml:"allow_cancel"`
}

// XEPRegister represents an in-band server stream module.
type XEPRegister struct {
	cfg        *Config
	stm        c2s.Stream
	registered bool
}

// New returns an in-band registration IQ handler.
func New(config *Config, stm c2s.Stream) *XEPRegister {
	return &XEPRegister{
		cfg: config,
		stm: stm,
	}
}

// AssociatedNamespaces returns namespaces associated
// with in-band registration module.
func (x *XEPRegister) AssociatedNamespaces() []string {
	return []string{registerNamespace}
}

// MatchesIQ returns whether or not an IQ should be
// processed by the in-band registration module.
func (x *XEPRegister) MatchesIQ(iq *xml.IQ) bool {
	return iq.Elements().ChildNamespace("query", registerNamespace) != nil
}

// ProcessIQ processes an in-band registration IQ
// taking according actions over the associated stream.
func (x *XEPRegister) ProcessIQ(iq *xml.IQ) {
	if !x.isValidToJid(iq.ToJID()) {
		x.stm.SendElement(iq.ForbiddenError())
		return
	}

	q := iq.Elements().ChildNamespace("query", registerNamespace)
	if !x.stm.IsAuthenticated() {
		if iq.IsGet() {
			if !x.cfg.AllowRegistration {
				x.stm.SendElement(iq.NotAllowedError())
				return
			}
			// ...send registration fields to requester entity...
			x.sendRegistrationFields(iq, q)
		} else if iq.IsSet() {
			if !x.registered {
				// ...register a new user...
				x.registerNewUser(iq, q)
			} else {
				// return a <not-acceptable/> stanza error if an entity attempts to register a second identity
				x.stm.SendElement(iq.NotAcceptableError())
			}
		} else {
			x.stm.SendElement(iq.BadRequestError())
		}
	} else if iq.IsSet() {
		if q.Elements().Child("remove") != nil {
			// remove user
			x.cancelRegistration(iq, q)
		} else {
			user := q.Elements().Child("username")
			password := q.Elements().Child("password")
			if user != nil && password != nil {
				// change password
				x.changePassword(password.Text(), user.Text(), iq)
			} else {
				x.stm.SendElement(iq.BadRequestError())
			}
		}
	} else {
		x.stm.SendElement(iq.BadRequestError())
	}
}

func (x *XEPRegister) sendRegistrationFields(iq *xml.IQ, query xml.XElement) {
	if query.Elements().Count() > 0 {
		x.stm.SendElement(iq.BadRequestError())
		return
	}
	result := iq.ResultIQ()
	q := xml.NewElementNamespace("query", registerNamespace)
	q.AppendElement(xml.NewElementName("username"))
	q.AppendElement(xml.NewElementName("password"))
	result.AppendElement(q)
	x.stm.SendElement(result)
}

func (x *XEPRegister) registerNewUser(iq *xml.IQ, query xml.XElement) {
	userEl := query.Elements().Child("username")
	passwordEl := query.Elements().Child("password")
	if userEl == nil || passwordEl == nil || len(userEl.Text()) == 0 || len(passwordEl.Text()) == 0 {
		x.stm.SendElement(iq.BadRequestError())
		return
	}
	exists, err := storage.Instance().UserExists(userEl.Text())
	if err != nil {
		log.Errorf("%v", err)
		x.stm.SendElement(iq.InternalServerError())
		return
	}
	if exists {
		x.stm.SendElement(iq.ConflictError())
		return
	}
	user := model.User{
		Username: userEl.Text(),
		Password: passwordEl.Text(),
	}
	if err := storage.Instance().InsertOrUpdateUser(&user); err != nil {
		log.Errorf("%v", err)
		x.stm.SendElement(iq.InternalServerError())
		return
	}
	x.stm.SendElement(iq.ResultIQ())
	x.registered = true
}

func (x *XEPRegister) cancelRegistration(iq *xml.IQ, query xml.XElement) {
	if !x.cfg.AllowCancel {
		x.stm.SendElement(iq.NotAllowedError())
		return
	}
	if query.Elements().Count() > 1 {
		x.stm.SendElement(iq.BadRequestError())
		return
	}
	if err := storage.Instance().DeleteUser(x.stm.Username()); err != nil {
		log.Error(err)
		x.stm.SendElement(iq.InternalServerError())
		return
	}
	x.stm.SendElement(iq.ResultIQ())
}

func (x *XEPRegister) changePassword(password string, username string, iq *xml.IQ) {
	if !x.cfg.AllowChange {
		x.stm.SendElement(iq.NotAllowedError())
		return
	}
	if username != x.stm.Username() {
		x.stm.SendElement(iq.NotAllowedError())
		return
	}
	if !x.stm.IsSecured() {
		// channel isn't safe enough to enable a password change
		x.stm.SendElement(iq.NotAuthorizedError())
		return
	}
	user, err := storage.Instance().FetchUser(username)
	if err != nil {
		log.Error(err)
		x.stm.SendElement(iq.InternalServerError())
		return
	}
	if user == nil {
		x.stm.SendElement(iq.ResultIQ())
		return
	}
	if user.Password != password {
		user.Password = password
		if err := storage.Instance().InsertOrUpdateUser(user); err != nil {
			log.Error(err)
			x.stm.SendElement(iq.InternalServerError())
			return
		}
	}
	x.stm.SendElement(iq.ResultIQ())
}

func (x *XEPRegister) isValidToJid(jid *xml.JID) bool {
	if x.stm.IsAuthenticated() {
		return jid.IsServer()
	}
	return jid.IsServer() || (jid.IsBare() && jid.Node() == x.stm.Username())
}
