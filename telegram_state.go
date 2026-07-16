package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	telegramStateSchemaVersion = 1
	telegramPairingTTL         = time.Hour
	telegramMaxPendingPairings = 3
	telegramPairingAlphabet    = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

type telegramPairingRequest struct {
	UserID    string    `json:"userId"`
	ChatID    string    `json:"chatId"`
	Code      string    `json:"code"`
	CreatedAt time.Time `json:"createdAt"`
	LastSeen  time.Time `json:"lastSeenAt"`
	Label     string    `json:"label,omitempty"`
}

type telegramState struct {
	SchemaVersion    int                      `json:"schemaVersion"`
	BotID            string                   `json:"botId,omitempty"`
	TokenFingerprint string                   `json:"tokenFingerprint,omitempty"`
	LastUpdateID     int64                    `json:"lastUpdateId,omitempty"`
	Approved         []string                 `json:"approved,omitempty"`
	Pending          []telegramPairingRequest `json:"pending,omitempty"`
}

func defaultTelegramState() telegramState {
	return telegramState{SchemaVersion: telegramStateSchemaVersion}
}

func telegramStateFile(profile string) string {
	return filepath.Join(profileDir(profile), "telegram-state.json")
}

func validTelegramNumericID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	value, err := strconv.ParseInt(id, 10, 64)
	return err == nil && value > 0
}

func readTelegramState(profile string) (telegramState, error) {
	path := telegramStateFile(profile)
	if err := rejectSymlinkPath(path); err != nil {
		return telegramState{}, errors.New("unsafe Telegram state path")
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultTelegramState(), nil
	}
	if err != nil {
		return telegramState{}, errors.New("could not read Telegram state")
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return telegramState{}, errors.New("Telegram state must be a regular file")
	} else if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return telegramState{}, errors.New("Telegram state permissions must be 0600")
	}
	var state telegramState
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return telegramState{}, errors.New("Telegram state is corrupt; access is denied until it is repaired")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return telegramState{}, errors.New("Telegram state has trailing data; access is denied")
	}
	if err := validateTelegramState(&state); err != nil {
		return telegramState{}, err
	}
	return state, nil
}

func validateTelegramState(state *telegramState) error {
	if state.SchemaVersion != telegramStateSchemaVersion {
		return fmt.Errorf("unsupported Telegram state schema %d", state.SchemaVersion)
	}
	if state.BotID != "" && !validTelegramNumericID(state.BotID) {
		return errors.New("Telegram state contains an invalid bot ID")
	}
	if state.LastUpdateID < 0 {
		return errors.New("Telegram state contains an invalid update offset")
	}
	if state.TokenFingerprint != "" {
		if len(state.TokenFingerprint) != 64 {
			return errors.New("Telegram state contains an invalid token fingerprint")
		}
		for _, r := range state.TokenFingerprint {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				return errors.New("Telegram state contains an invalid token fingerprint")
			}
		}
	}
	seen := make(map[string]bool)
	approved := make([]string, 0, len(state.Approved))
	for _, id := range state.Approved {
		if !validTelegramNumericID(id) {
			return errors.New("Telegram state contains an invalid approved user ID")
		}
		if !seen[id] {
			seen[id] = true
			approved = append(approved, id)
		}
	}
	sort.Strings(approved)
	state.Approved = approved
	seenCodes := make(map[string]bool)
	seenPendingUsers := make(map[string]bool)
	if len(state.Pending) > telegramMaxPendingPairings {
		return errors.New("Telegram state contains too many pairing requests")
	}
	for i := range state.Pending {
		request := &state.Pending[i]
		if !validTelegramNumericID(request.UserID) || !validTelegramNumericID(request.ChatID) || len(request.Code) != 8 {
			return errors.New("Telegram state contains an invalid pairing request")
		}
		if request.CreatedAt.IsZero() || request.LastSeen.IsZero() || request.LastSeen.Before(request.CreatedAt) {
			return errors.New("Telegram state contains invalid pairing timestamps")
		}
		request.Label = safeTelegramPairingLabel(request.Label)
		for _, r := range request.Code {
			if !strings.ContainsRune(telegramPairingAlphabet, r) {
				return errors.New("Telegram state contains an invalid pairing code")
			}
		}
		if seenCodes[request.Code] || seenPendingUsers[request.UserID] {
			return errors.New("Telegram state contains duplicate pairing requests")
		}
		seenCodes[request.Code] = true
		seenPendingUsers[request.UserID] = true
	}
	return nil
}

func updateTelegramState(profile string, fn func(*telegramState) error) (telegramState, error) {
	var state telegramState
	err := withProfileLock(profile, "telegram.lock", func() error {
		var err error
		state, err = readTelegramState(profile)
		if err != nil {
			return err
		}
		if err := fn(&state); err != nil {
			return err
		}
		if err := validateTelegramState(&state); err != nil {
			return err
		}
		return privateAtomicWrite(telegramStateFile(profile), state)
	})
	return state, err
}

func pruneTelegramPairings(state *telegramState, now time.Time) {
	pending := state.Pending[:0]
	for _, request := range state.Pending {
		if now.Sub(request.CreatedAt) < telegramPairingTTL && !now.Before(request.CreatedAt) {
			pending = append(pending, request)
		}
	}
	state.Pending = pending
}

func createTelegramPairing(profile, userID, chatID, label string, now time.Time) (string, bool, error) {
	var code string
	created := false
	_, err := updateTelegramState(profile, func(state *telegramState) error {
		pruneTelegramPairings(state, now)
		for i := range state.Pending {
			if state.Pending[i].UserID == userID {
				state.Pending[i].LastSeen = now
				code = state.Pending[i].Code
				return nil
			}
		}
		if len(state.Pending) >= telegramMaxPendingPairings {
			return errors.New("pairing request limit reached; ask the operator to approve or wait for an existing code to expire")
		}
		var err error
		code, err = newTelegramPairingCode()
		if err != nil {
			return err
		}
		state.Pending = append(state.Pending, telegramPairingRequest{
			UserID: userID, ChatID: chatID, Code: code, CreatedAt: now, LastSeen: now, Label: label,
		})
		created = true
		return nil
	})
	return code, created, err
}

func newTelegramPairingCode() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("could not generate Telegram pairing code")
	}
	for i := range raw {
		raw[i] = telegramPairingAlphabet[int(raw[i])%len(telegramPairingAlphabet)]
	}
	return string(raw), nil
}

func approveTelegramPairing(profile, code string, now time.Time) (string, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) != 8 {
		return "", errors.New("pairing code must be 8 characters")
	}
	for _, r := range code {
		if !strings.ContainsRune(telegramPairingAlphabet, r) {
			return "", errors.New("pairing code contains invalid characters")
		}
	}
	var approved string
	_, err := updateTelegramState(profile, func(state *telegramState) error {
		pruneTelegramPairings(state, now)
		pending := state.Pending[:0]
		for _, request := range state.Pending {
			if request.Code == code && approved == "" {
				approved = request.UserID
				continue
			}
			pending = append(pending, request)
		}
		if approved == "" {
			return errors.New("pairing code is unknown or expired")
		}
		for _, id := range state.Approved {
			if id == approved {
				state.Pending = pending
				return nil
			}
		}
		state.Approved = append(state.Approved, approved)
		state.Pending = pending
		return nil
	})
	return approved, err
}

func telegramUserAuthorized(cfg telegramConfig, state telegramState, userID string) bool {
	for _, id := range cfg.AllowFrom {
		if id == userID {
			return true
		}
	}
	if cfg.DMPolicy != telegramPolicyPairing {
		return false
	}
	for _, id := range state.Approved {
		if id == userID {
			return true
		}
	}
	return false
}

func bindTelegramState(profile, botID, fingerprint string) (telegramState, error) {
	return updateTelegramState(profile, func(state *telegramState) error {
		botChanged := state.BotID != botID
		tokenChanged := state.TokenFingerprint != fingerprint
		if botChanged {
			state.Approved = nil
			// Update IDs belong to a bot. A genuinely different bot starts a
			// fresh stream and must not inherit either authority or its offset.
			state.LastUpdateID = 0
		}
		if botChanged || tokenChanged {
			state.BotID = botID
			state.TokenFingerprint = fingerprint
			state.Pending = nil
		}
		return nil
	})
}

func recordTelegramUpdate(profile string, updateID int64) error {
	_, err := updateTelegramState(profile, func(state *telegramState) error {
		if updateID > state.LastUpdateID {
			state.LastUpdateID = updateID
		}
		return nil
	})
	return err
}
