package push

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	fcmOAuthScope   = "https://www.googleapis.com/auth/firebase.messaging"
	defaultTokenURI = "https://oauth2.googleapis.com/token"
)

type fcmServiceAccount struct {
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

type FCMSender struct {
	projectID   string
	clientEmail string
	privateKey  *rsa.PrivateKey
	tokenURI    string
	httpClient  *http.Client

	mu          sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

func NewFCMSender(projectID, serviceAccountFile string) (*FCMSender, error) {
	if strings.TrimSpace(serviceAccountFile) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(serviceAccountFile)
	if err != nil {
		return nil, fmt.Errorf("read FCM service account: %w", err)
	}
	var account fcmServiceAccount
	if err := json.Unmarshal(raw, &account); err != nil {
		return nil, fmt.Errorf("parse FCM service account: %w", err)
	}
	if strings.TrimSpace(projectID) == "" {
		projectID = account.ProjectID
	}
	if projectID == "" || account.ClientEmail == "" || account.PrivateKey == "" {
		return nil, errors.New("FCM service account is missing project_id, client_email, or private_key")
	}
	privateKey, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(account.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("parse FCM private key: %w", err)
	}
	tokenURI := strings.TrimSpace(account.TokenURI)
	if tokenURI == "" {
		tokenURI = defaultTokenURI
	}
	return &FCMSender{
		projectID:   projectID,
		clientEmail: account.ClientEmail,
		privateKey:  privateKey,
		tokenURI:    tokenURI,
		httpClient:  &http.Client{Timeout: 12 * time.Second},
	}, nil
}

func (s *FCMSender) Provider() string { return "fcm" }

func (s *FCMSender) Send(ctx context.Context, notification Notification) error {
	token, err := s.oauthAccessToken(ctx)
	if err != nil {
		return err
	}
	data := map[string]string{
		"type":         "room_message",
		"message_id":   notification.MessageID,
		"room_id":      notification.RoomID,
		"room_name":    notification.RoomName,
		"sender_name":  notification.SenderName,
		"body":         notification.Body,
		"unread_count": strconv.Itoa(max(notification.UnreadCount, 1)),
	}
	payload := map[string]any{
		"message": map[string]any{
			"token": notification.Token,
			"data":  data,
			"android": map[string]any{
				"priority":     "HIGH",
				"ttl":          "604800s",
				"collapse_key": "gang-chat-room-" + notification.RoomID,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode FCM message: %w", err)
	}
	endpoint := "https://fcm.googleapis.com/v1/projects/" +
		url.PathEscape(s.projectID) + "/messages:send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send FCM message: %w", err)
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	responseText := string(responseBody)
	if resp.StatusCode == http.StatusNotFound ||
		strings.Contains(responseText, "UNREGISTERED") {
		return ErrUnregisteredToken
	}
	return fmt.Errorf("FCM returned %s: %s", resp.Status, strings.TrimSpace(responseText))
}

func (s *FCMSender) oauthAccessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accessToken != "" && time.Until(s.tokenExpiry) > time.Minute {
		return s.accessToken, nil
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":   s.clientEmail,
		"scope": fcmOAuthScope,
		"aud":   s.tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	assertion, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(s.privateKey)
	if err != nil {
		return "", fmt.Errorf("sign FCM OAuth assertion: %w", err)
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request FCM OAuth token: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("FCM OAuth returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var decoded struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.AccessToken == "" {
		return "", errors.New("FCM OAuth response did not contain an access token")
	}
	if decoded.ExpiresIn <= 0 {
		decoded.ExpiresIn = 3600
	}
	s.accessToken = decoded.AccessToken
	s.tokenExpiry = now.Add(time.Duration(decoded.ExpiresIn) * time.Second)
	return s.accessToken, nil
}
