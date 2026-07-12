package auth

type RegisterRequest struct {
	Username               string `json:"username" binding:"required"`
	Email                  string `json:"email" binding:"required,email"`
	Password               string `json:"password" binding:"required,min=8,max=256"`
	EmailVerificationToken string `json:"email_verification_token" binding:"required"`
}

type EmailVerificationRequest struct {
	Email string `json:"email" binding:"required,email,max=254"`
}

type EmailVerificationChallengeRequest struct {
	ChallengeID string `json:"challenge_id" binding:"required"`
}

type VerifyEmailVerificationRequest struct {
	ChallengeID string `json:"challenge_id" binding:"required"`
	Code        string `json:"code" binding:"required,len=6"`
}

type EmailVerificationChallengeResponse struct {
	ChallengeID string `json:"challenge_id"`
	RetryAfter  int64  `json:"retry_after"`
}

type EmailVerificationInspectionResponse struct {
	CanSend     bool    `json:"can_send"`
	ChallengeID *string `json:"challenge_id,omitempty"`
	RetryAfter  int64   `json:"retry_after"`
}

type EmailVerificationResponse struct {
	VerificationToken string `json:"verification_token"`
}

type LoginRequest struct {
	Login    string `json:"login" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

type LogoutRequest struct {
	RefreshToken *string `json:"refresh_token"`
}

type ChangePasswordRequest struct {
	CurrentPassword     string `json:"current_password"`
	NewPassword         string `json:"new_password" binding:"required,min=8,max=256"`
	RevokeOtherSessions *bool  `json:"revoke_other_sessions"`
}

type StartPasswordResetRequest struct {
	Login string `json:"login" binding:"required,max=320"`
}

type PasswordResetChallengeRequest struct {
	ChallengeID string `json:"challenge_id" binding:"required"`
}

type VerifyPasswordResetRequest struct {
	ChallengeID string `json:"challenge_id" binding:"required"`
	Code        string `json:"code" binding:"required,len=6"`
}

type CompletePasswordResetRequest struct {
	ResetToken  string `json:"reset_token" binding:"required"`
	NewPassword string `json:"new_password" binding:"required,min=8,max=256"`
}

type ClaimPasswordResetRequest struct {
	ResetToken string `json:"reset_token" binding:"required"`
}

type PasswordResetChallengeResponse struct {
	ChallengeID string `json:"challenge_id"`
	MaskedEmail string `json:"masked_email"`
	RetryAfter  int64  `json:"retry_after"`
}

type PasswordResetInspectionResponse struct {
	CanSend     bool    `json:"can_send"`
	ChallengeID *string `json:"challenge_id,omitempty"`
	MaskedEmail string  `json:"masked_email"`
	RetryAfter  int64   `json:"retry_after"`
}

type PasswordResetVerificationResponse struct {
	ResetToken string `json:"reset_token"`
}

type UpdateAccountRequest struct {
	Username          *string `json:"username"`
	Email             *string `json:"email"`
	EmailPublic       *bool   `json:"email_public"`
	PhoneNumber       *string `json:"phone_number"`
	PhoneNumberPublic *bool   `json:"phone_number_public"`
	Language          *string `json:"language"`
}

type UpdateProfileRequest struct {
	DisplayName      *string `json:"display_name"`
	Bio              *string `json:"bio"`
	Gender           *string `json:"gender"`
	AvatarAssetID    *string `json:"avatar_asset_id"`
	DefaultAvatarKey *string `json:"default_avatar_key"`
}

type UpdateAudioSettingsRequest struct {
	DefaultAudioInputVolume     *int `json:"default_audio_input_volume"`
	DefaultAudioOutputVolume    *int `json:"default_audio_output_volume"`
	LiveMicInputVolume          *int `json:"live_mic_input_volume"`
	LiveVoiceOutputVolume       *int `json:"live_voice_output_volume"`
	LiveScreenShareOutputVolume *int `json:"live_screen_share_output_volume"`
	LiveMusicOutputVolume       *int `json:"live_music_output_volume"`
}

type AuthResponse struct {
	AccessToken          string       `json:"access_token"`
	AccessTokenExpiresAt string       `json:"access_token_expires_at"`
	RefreshToken         string       `json:"refresh_token"`
	ExpiresIn            int64        `json:"expires_in,omitempty"`
	User                 UserResponse `json:"user"`
}

type UserResponse struct {
	ID                  string  `json:"id"`
	UID                 string  `json:"uid,omitempty"`
	Username            string  `json:"username"`
	DisplayName         string  `json:"display_name"`
	Bio                 string  `json:"bio"`
	Gender              string  `json:"gender"`
	Email               string  `json:"email,omitempty"`
	EmailVerified       *bool   `json:"email_verified,omitempty"`
	EmailPublic         bool    `json:"email_public"`
	PhoneNumber         string  `json:"phone_number,omitempty"`
	PhoneNumberPublic   bool    `json:"phone_number_public"`
	Language            string  `json:"language"`
	AvatarURL           *string `json:"avatar_url"`
	DefaultAvatarKey    string  `json:"default_avatar_key"`
	IsSuperuser         bool    `json:"is_superuser,omitempty"`
	Status              string  `json:"status,omitempty"`
	UsernameUpdatedAt   *string `json:"username_updated_at,omitempty"`
	CanChangeUsernameAt *string `json:"can_change_username_at,omitempty"`
	CreatedAt           string  `json:"created_at,omitempty"`
}

type MessageResponse struct {
	OK bool `json:"ok"`
}

type AvailabilityResponse struct {
	Available bool `json:"available"`
}

type SessionResponse struct {
	ID         string  `json:"id"`
	UserAgent  *string `json:"user_agent"`
	IPAddress  *string `json:"ip_address"`
	Location   string  `json:"location"`
	CreatedAt  int64   `json:"created_at"`
	LastUsedAt int64   `json:"last_used_at"`
	ExpiresAt  int64   `json:"expires_at"`
	RevokedAt  *int64  `json:"revoked_at,omitempty"`
	IsCurrent  bool    `json:"is_current"`
}

type AudioSettingsResponse struct {
	DefaultAudioInputVolume     int    `json:"default_audio_input_volume"`
	DefaultAudioOutputVolume    int    `json:"default_audio_output_volume"`
	LiveMicInputVolume          int    `json:"live_mic_input_volume"`
	LiveVoiceOutputVolume       int    `json:"live_voice_output_volume"`
	LiveScreenShareOutputVolume int    `json:"live_screen_share_output_volume"`
	LiveMusicOutputVolume       int    `json:"live_music_output_volume"`
	UpdatedAt                   string `json:"updated_at"`
}

type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	RequestID string            `json:"request_id,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"`
}
