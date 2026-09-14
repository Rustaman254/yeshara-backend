// Package staff is the four-eyes-times-two approval chain's account type:
// Manager A, Manager B, Trustee A, and Trustee B are each a distinct,
// separately-authenticated login — not four permission flags on one admin
// account. That's the whole point of segregation of duties: a Manager B
// credential cannot act as Trustee A no matter what, because there is no
// code path that lets a *manager_b session* invoke a *trustee_a
// transition* — see offerings.ManagerBReview/TrusteeAApprove and how
// internal/api dispatches on Role, not on a URL a caller could just hit
// directly.
//
// Structurally identical to internal/owners (bcrypt password, random-token
// session in Mongo with a TTL index) — deliberately not reusing it, since
// "an owner viewing their stake" and "a trustee tokenizing an asset" are
// different trust boundaries that happen to share a login shape.
//
// Staff accounts are provisioned by an admin (POST /api/v1/admin/staff),
// never self-registered — these are named operational roles a real
// institution assigns, not something anyone can sign up for.
package staff

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/crypto/bcrypt"

	"yeshara.network/marketplace/internal/database"
)

// Role is one of the four seats in Yeshara's tokenization approval chain:
// Manager A (maker) reviews a submission, Manager B (checker) countersigns
// it, Trustee A approves issuance, Trustee B actually tokenizes (mints)
// and hands the asset to the seller.
type Role string

const (
	RoleManagerA Role = "manager_a"
	RoleManagerB Role = "manager_b"
	RoleTrusteeA Role = "trustee_a"
	RoleTrusteeB Role = "trustee_b"
)

// ValidRole reports whether r is one of the four known seats — checked at
// account creation so a typo'd role string can't silently create an
// unreachable account (nothing would ever route work to it).
func ValidRole(r Role) bool {
	switch r {
	case RoleManagerA, RoleManagerB, RoleTrusteeA, RoleTrusteeB:
		return true
	default:
		return false
	}
}

type Staff struct {
	ID           primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Email        string             `bson:"email" json:"email"`
	PasswordHash string             `bson:"password_hash" json:"-"`
	FullName     string             `bson:"full_name" json:"fullName"`
	Role         Role               `bson:"role" json:"role"`
	CreatedAt    time.Time          `bson:"created_at" json:"createdAt"`
}

type Session struct {
	Token     string             `bson:"_id"`
	StaffID   primitive.ObjectID `bson:"staff_id"`
	ExpiresAt time.Time          `bson:"expires_at"`
	CreatedAt time.Time          `bson:"created_at"`
}

const sessionTTL = 12 * time.Hour

var (
	ErrEmailTaken         = errors.New("staff: an account with this email already exists")
	ErrInvalidRole        = errors.New("staff: role must be one of manager_a, manager_b, trustee_a, trustee_b")
	ErrInvalidCredentials = errors.New("staff: invalid email or password")
	ErrNotFound           = errors.New("staff: not found")
	ErrSessionExpired     = errors.New("staff: session expired or invalid")
)

func collection() *mongo.Collection         { return database.GetDB().Collection("staff") }
func sessionsCollection() *mongo.Collection { return database.GetDB().Collection("staff_sessions") }

func EnsureIndexes(ctx context.Context) error {
	_, err := collection().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "email", Value: 1}}, Options: options.Index().SetUnique(true),
	})
	if err != nil {
		return err
	}
	_, err = sessionsCollection().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(0),
	})
	return err
}

// Create provisions a new named seat — admin-only (see
// internal/api's requireAdmin gate on POST /api/v1/admin/staff).
func Create(ctx context.Context, email, password, fullName string, role Role) (*Staff, error) {
	if !ValidRole(role) {
		return nil, ErrInvalidRole
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	s := &Staff{Email: email, PasswordHash: string(hash), FullName: fullName, Role: role, CreatedAt: time.Now().UTC()}
	res, err := collection().InsertOne(ctx, s)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil, ErrEmailTaken
		}
		return nil, err
	}
	s.ID = res.InsertedID.(primitive.ObjectID)
	return s, nil
}

func List(ctx context.Context) ([]Staff, error) {
	cur, err := collection().Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []Staff
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Delete removes a staff account and invalidates every session it holds —
// an offboarding action, not something a seat can do to itself.
func Delete(ctx context.Context, id primitive.ObjectID) error {
	res, err := collection().DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return ErrNotFound
	}
	_, err = sessionsCollection().DeleteMany(ctx, bson.M{"staff_id": id})
	return err
}

func GetByEmail(ctx context.Context, email string) (*Staff, error) {
	var s Staff
	err := collection().FindOne(ctx, bson.M{"email": email}).Decode(&s)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	return &s, err
}

func GetByID(ctx context.Context, id primitive.ObjectID) (*Staff, error) {
	var s Staff
	err := collection().FindOne(ctx, bson.M{"_id": id}).Decode(&s)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	return &s, err
}

func Login(ctx context.Context, email, password string) (*Staff, string, error) {
	s, err := GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, "", ErrInvalidCredentials
		}
		return nil, "", err
	}
	if bcrypt.CompareHashAndPassword([]byte(s.PasswordHash), []byte(password)) != nil {
		return nil, "", ErrInvalidCredentials
	}
	token, err := createSession(ctx, s.ID)
	if err != nil {
		return nil, "", err
	}
	return s, token, nil
}

func createSession(ctx context.Context, staffID primitive.ObjectID) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	now := time.Now().UTC()
	_, err := sessionsCollection().InsertOne(ctx, Session{
		Token: token, StaffID: staffID, ExpiresAt: now.Add(sessionTTL), CreatedAt: now,
	})
	return token, err
}

// ValidateSession returns the staff member a session token belongs to, or
// ErrSessionExpired if it's missing/expired.
func ValidateSession(ctx context.Context, token string) (*Staff, error) {
	var sess Session
	err := sessionsCollection().FindOne(ctx, bson.M{"_id": token}).Decode(&sess)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrSessionExpired
	}
	if err != nil {
		return nil, err
	}
	if time.Now().UTC().After(sess.ExpiresAt) {
		return nil, ErrSessionExpired
	}
	return GetByID(ctx, sess.StaffID)
}

func Logout(ctx context.Context, token string) error {
	_, err := sessionsCollection().DeleteOne(ctx, bson.M{"_id": token})
	return err
}
