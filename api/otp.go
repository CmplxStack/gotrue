package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/netlify/gotrue/crypto"
	"github.com/netlify/gotrue/models"
	"github.com/netlify/gotrue/storage"
	"github.com/sethvargo/go-password/password"
)

type OtpParams struct {
	Email string `json:"email"`
	Phone string `json:"phone"`
}

type SmsParams struct {
	Phone string `json:"phone"`
}

func (a *API) Otp(w http.ResponseWriter, r *http.Request) error {
	params := &OtpParams{}
	body, err := ioutil.ReadAll(r.Body)
	jsonDecoder := json.NewDecoder(bytes.NewReader(body))
	if err = jsonDecoder.Decode(params); err != nil {
		return badRequestError("Could not read verification params: %v", err)
	}
	if params.Email != "" && params.Phone != "" {
		return badRequestError("Only an emaill address or phone number should be provided")
	}

	r.Body = ioutil.NopCloser(strings.NewReader(string(body)))
	if params.Email != "" {
		return a.MagicLink(w, r)
	} else if params.Phone != "" {
		return a.SmsOtp(w, r)
	}

	return otpError("unsupported_otp_type", "")
}

// SmsOtp sends the user an otp via sms
func (a *API) SmsOtp(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	instanceID := getInstanceID(ctx)
	params := &SmsParams{}
	jsonDecoder := json.NewDecoder(r.Body)
	if err := jsonDecoder.Decode(params); err != nil {
		return badRequestError("Could not read sms otp params: %v", err)
	}

	params.Phone = a.formatPhoneNumber(params.Phone)

	if isValid := a.validateE164Format(params.Phone); !isValid {
		return badRequestError("Invalid format: Phone number should follow the E.164 format")
	}

	aud := a.requestAud(ctx, r)

	user, uerr := models.FindUserByPhoneAndAudience(a.db, instanceID, params.Phone, aud)
	if uerr != nil {
		// if user does not exists, sign up the user
		if models.IsNotFoundError(uerr) {
			password, err := password.Generate(64, 10, 0, false, true)
			if err != nil {
				internalServerError("error creating user").WithInternalError(err)
			}
			newBodyContent := `{"phone":"` + params.Phone + `","password":"` + password + `"}`
			r.Body = ioutil.NopCloser(strings.NewReader(newBodyContent))
			r.ContentLength = int64(len(newBodyContent))

			fakeResponse := &responseStub{}

			if err := a.Signup(fakeResponse, r); err != nil {
				return err
			}
			return sendJSON(w, http.StatusOK, make(map[string]string))
		}
		return internalServerError("Database error finding user").WithInternalError(uerr)
	}

	err := a.db.Transaction(func(tx *storage.Connection) error {
		if err := models.NewAuditLogEntry(tx, instanceID, user, models.UserRecoveryRequestedAction, nil); err != nil {
			return err
		}

		if err := a.sendPhoneConfirmation(ctx, user, params.Phone); err != nil {
			return internalServerError("Error sending confirmation sms").WithInternalError(err)
		}
		return nil
	})

	if err != nil {
		return err
	}

	return sendJSON(w, http.StatusOK, make(map[string]string))
}

func (a *API) createNewTotpAuth(ctx context.Context, conn *storage.Connection, user *models.User, phone string) (*models.TotpAuth, error) {
	instanceID := getInstanceID(ctx)
	config := a.getConfig(ctx)

	key, err := crypto.GenerateTotpKey(config, phone)
	if err != nil {
		return nil, internalServerError("error creating totp key").WithInternalError(err)
	}
	totpAuth, err := models.NewTotpAuth(instanceID, user.ID, key.String())

	terr := conn.Transaction(func(tx *storage.Connection) error {
		verrs, err := tx.ValidateAndCreate(totpAuth)
		if verrs.Count() > 0 {
			return internalServerError("Database error saving new totp auth data").WithInternalError(verrs)
		}
		if err != nil {
			return internalServerError("Database error saving new totp auth data").WithInternalError(err)
		}
		return nil
	})
	if terr != nil {
		return nil, terr
	}
	return totpAuth, nil
}
