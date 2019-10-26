package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/fiatjaf/etleneum/types"
	"github.com/fiatjaf/go-lnurl"
	"github.com/lucsky/cuid"
	"gopkg.in/antage/eventsource.v1"
)

func lnurlSession(w http.ResponseWriter, r *http.Request) {
	var es eventsource.EventSource
	session := r.URL.Query().Get("session")

	if session == "" {
		session = lnurl.RandomK1()
	} else {
		// check session validity as k1
		b, err := hex.DecodeString(session)
		if err != nil || len(b) != 32 {
			session = lnurl.RandomK1()
		} else {
			// finally try to fetch an existing stream
			ies, ok := userstreams.Get(session)
			if ok {
				es = ies.(eventsource.EventSource)
			}
		}
	}

	if es == nil {
		es = eventsource.New(
			&eventsource.Settings{
				Timeout:        5 * time.Second,
				CloseOnTimeout: true,
				IdleTimeout:    300 * time.Minute,
			},
			func(r *http.Request) [][]byte {
				return [][]byte{
					[]byte("X-Accel-Buffering: no"),
					[]byte("Cache-Control: no-cache"),
					[]byte("Content-Type: text/event-stream"),
					[]byte("Connection: keep-alive"),
					[]byte("Access-Control-Allow-Origin: *"),
				}
			},
		)
		userstreams.Set(session, es)
		go func() {
			for {
				time.Sleep(25 * time.Second)
				es.SendEventMessage("", "keepalive", "")
			}
		}()
	}

	go func() {
		time.Sleep(1 * time.Second)
		es.SendRetryMessage(3 * time.Second)
	}()

	accountId := rds.Get("auth-session:" + session).Val()
	if accountId != "" {
		// we're logged already, so send account information
		go func() {
			time.Sleep(2 * time.Second)
			var acct types.Account
			err := pg.Get(&acct, `SELECT `+types.ACCOUNTFIELDS+` FROM accounts WHERE id = $1`, accountId)
			if err != nil {
				log.Error().Err(err).Str("session", session).Str("id", accountId).
					Msg("failed to load account from session")
				return
			}
			es.SendEventMessage(`{"account": "`+acct.Id+`", "balance": "`+strconv.Itoa(acct.Balance)+`"}`, "auth", "")
			log.Debug().Str("account", accountId).Str("session", session).Str("type", "auth").
				Msg("dispatched session message")
		}()

		// also renew his session
		rds.Expire("auth-session:"+session, time.Hour*24*30)
	}

	// always send lnurls because we need lnurl-withdraw even if we're logged already, obviously
	go func() {
		time.Sleep(2 * time.Second)
		auth, _ := lnurl.LNURLEncode(s.ServiceURL + "/lnurl/auth?tag=login&k1=" + session)
		withdraw, _ := lnurl.LNURLEncode(s.ServiceURL + "/lnurl/withdraw?session=" + session)
		es.SendEventMessage(`{"auth": "`+auth+`", "withdraw": "`+withdraw+`"}`, "lnurls", "")
		log.Debug().Str("account", accountId).Str("session", session).Str("type", "lnurls").
			Msg("dispatched session message")
	}()

	es.ServeHTTP(w, r)
}

func lnurlAuth(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	k1 := params.Get("k1")
	sig := params.Get("sig")
	key := params.Get("key")

	if ok, err := lnurl.VerifySignature(k1, sig, key); !ok {
		log.Debug().Err(err).Str("k1", k1).Str("sig", sig).Str("key", key).
			Msg("failed to verify lnurl-auth signature")
		json.NewEncoder(w).Encode(lnurl.ErrorResponse("signature verification failed."))
		return
	}

	session := k1
	log.Debug().Str("session", session).Str("pubkey", key).Msg("valid login")

	// there must be a valid auth session (meaning an eventsource client) one otherwise something is wrong
	ies, ok := userstreams.Get(session)
	if !ok {
		json.NewEncoder(w).Encode(lnurl.ErrorResponse("there's no browser session to authorize."))
		return
	}

	// get the account id from the pubkey
	var acct types.Account
	err = pg.Get(&acct, `
INSERT INTO accounts (id, lnurl_key) VALUES ($1, $2)
ON CONFLICT (lnurl_key)
  DO UPDATE SET lnurl_key = $2
  RETURNING `+types.ACCOUNTFIELDS+`
    `, "a"+cuid.Slug(), key)
	if err != nil {
		log.Error().Err(err).Str("key", key).Msg("failed to ensure account")
		json.NewEncoder(w).Encode(lnurl.ErrorResponse("failed to ensure account with key " + key + "."))
		return
	}

	// assign the account id to this session on redis
	if rds.Set("auth-session:"+session, acct.Id, time.Hour*24*30).Err() != nil {
		json.NewEncoder(w).Encode(lnurl.ErrorResponse("failed to save session."))
		return
	}

	// notify browser
	ies.(eventsource.EventSource).SendEventMessage(`{"session": "`+k1+`", "account": "`+acct.Id+`", "balance": "`+strconv.Itoa(acct.Balance)+`"}`, "auth", "")
	log.Debug().Str("account", acct.Id).Str("session", k1).Str("type", "auth").
		Msg("dispatched session message")

	json.NewEncoder(w).Encode(lnurl.OkResponse())
}

func refreshBalance(w http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("session")

	// get account id from session
	accountId, err := rds.Get("auth-session:" + session).Result()
	if err != nil {
		log.Error().Err(err).Str("session", session).Msg("failed to get session from redis on refresh")
		w.WriteHeader(500)
		return
	}

	// get balance
	var balance int
	err = pg.Get(&balance, "SELECT accounts.balance FROM accounts WHERE id = $1", accountId)
	if err != nil {
		w.WriteHeader(500)
		return
	}

	if ies, ok := userstreams.Get(session); ok {
		ies.(eventsource.EventSource).SendEventMessage(`{"account": "`+accountId+`", "balance": "`+strconv.Itoa(balance)+`"}`, "auth", "")
		log.Debug().Str("account", accountId).Str("session", session).Str("type", "auth").
			Msg("dispatched session message")
	}

	w.WriteHeader(200)
}

func lnurlWithdraw(w http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("session")

	// get account id from session
	accountId, err := rds.Get("auth-session:" + session).Result()
	if err != nil {
		log.Error().Err(err).Str("session", session).Msg("failed to get session from redis on withdraw")
		json.NewEncoder(w).Encode(lnurl.ErrorResponse("lnurl session " + session + " has expired."))
		return
	}

	// get balance
	var balance int
	err = pg.Get(&balance, "SELECT accounts.balance FROM accounts WHERE id = $1", accountId)
	if err != nil {
		json.NewEncoder(w).Encode(lnurl.ErrorResponse("error fetching " + accountId + " balance."))
		return
	}

	if balance < 10000 {
		json.NewEncoder(w).Encode(lnurl.ErrorResponse("the minimum withdrawal is 10 sat, your balance is " + strconv.Itoa(balance) + " msat."))
		return
	}

	json.NewEncoder(w).Encode(lnurl.LNURLWithdrawResponse{
		LNURLResponse:      lnurl.LNURLResponse{Status: "OK"},
		Callback:           fmt.Sprintf("%s/lnurl/withdraw/callback", s.ServiceURL),
		K1:                 session,
		MaxWithdrawable:    int64(balance),
		MinWithdrawable:    10000,
		DefaultDescription: fmt.Sprintf("etleneum.com %s balance withdraw", accountId),
		Tag:                "withdrawRequest",
	})
}

func lnurlWithdrawCallback(w http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("k1")
	bolt11 := r.URL.Query().Get("pr")

	// get account id from session
	accountId, err := rds.Get("auth-session:" + session).Result()
	if err != nil {
		json.NewEncoder(w).Encode(lnurl.ErrorResponse("lnurl session " + session + " has expired."))
		return
	}

	// start withdrawal transaction
	txn, err := pg.BeginTxx(context.TODO(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		json.NewEncoder(w).Encode(lnurl.ErrorResponse("internal database error."))
		return
	}
	defer txn.Rollback()

	if s.FreeMode {
		json.NewEncoder(w).Encode(lnurl.OkResponse())
		return
	}

	// decode invoice
	inv, err := ln.Call("decodepay", bolt11)
	if err != nil {
		json.NewEncoder(w).Encode(lnurl.ErrorResponse("failed to decode invoice."))
		return
	}
	amount := inv.Get("msatoshi").Int()

	log.Debug().Str("bolt11", bolt11).Str("account", accountId).Int64("amount", amount).
		Msg("got a withdraw payment request")

	// add a pending withdrawal
	_, err = txn.Exec(`
INSERT INTO withdrawals (account_id, msatoshi, fulfilled, bolt11)
VALUES ($1, $2, false, $3)
    `, accountId, amount, bolt11)
	if err != nil {
		log.Warn().Err(err).Msg("error inserting withdrawal")
		json.NewEncoder(w).Encode(lnurl.ErrorResponse("database error."))
		return
	}

	// check balance afterwards
	var balance int
	err = txn.Get(&balance, "SELECT accounts.balance FROM accounts WHERE id = $1", accountId)
	if err != nil {
		json.NewEncoder(w).Encode(lnurl.ErrorResponse("database error."))
		return
	}
	if balance < 0 {
		json.NewEncoder(w).Encode(lnurl.ErrorResponse("insufficient balance."))
		return
	}

	log.Debug().Int("balance after", balance).Msg("will fulfill")

	err = txn.Commit()
	if err != nil {
		log.Warn().Err(err).Msg("error commiting withdrawal")
		json.NewEncoder(w).Encode(lnurl.ErrorResponse("database error."))
		return
	}

	// actually send the payment
	go func() {
		payresp, err := ln.Call("waitpay", bolt11)
		log.Debug().Err(err).Str("resp", payresp.String()).Str("account", accountId).Str("bolt11", bolt11).
			Msg("withdraw waitpay result")

		if err == nil {
			// mark as fulfilled
			_, err := pg.Exec(`UPDATE withdrawals SET fulfilled = true WHERE bolt11 = $1`, bolt11)
			if err != nil {
				log.Error().Err(err).Str("accountId", accountId).Msg("error marking payment as fulfilled")
			}

			// notify browser
			if ies, ok := userstreams.Get(session); ok {
				ies.(eventsource.EventSource).SendEventMessage(
					`{"amount": `+strconv.Itoa(int(amount))+`, "new_balance": `+strconv.Itoa(balance)+`}`,
					"withdraw", "")
				log.Debug().Str("account", accountId).Str("session", session).Str("type", "withdraw").
					Msg("dispatched session message")
			}
		}

	}()

	json.NewEncoder(w).Encode(lnurl.OkResponse())
}

func logout(w http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("session")
	rds.Del("auth-session:" + session)
	userstreams.Remove(session)
	w.WriteHeader(200)
}
