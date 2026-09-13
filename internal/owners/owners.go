// Package owners is Yeshara's account type for the real-world owner of a
// tokenized asset — the person entitled to the reserved-owner stake
// offerings.Approve sets aside (see ReservedOwnerPct). Self-service
// email/password login, structurally identical to internal/investors —
// deliberately a separate account namespace rather than reusing Investor,
// since an owner isn't buying units, they're viewing a stake an admin
// linked to their account after verifying real-world ownership off-platform
// (the same trust model as KYC approval).
package owners

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

type Owner struct {
	ID           primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Email        string             `bson:"email" json:"email"`
	PasswordHash string             `bson:"password_hash" json:"-"`
	FullName     string             `bson:"full_name" json:"fullName"`
	CreatedAt    time.Time          `bson:"created_at" json:"createdAt"`
}

type Session struct {
	Token     string             `bson:"_id"`
	OwnerID   primitive.ObjectID `bson:"owner_id"`
	ExpiresAt time.Time          `bson:"expires_at"`
	CreatedAt time.Time          `bson:"created_at"`
}

const sessionTTL = 30 * 24 * time.Hour

var (
	ErrEmailTaken         = errors.New("owners: an account with this email already exists")
	ErrInvalidCredentials = errors.New("owners: invalid email or password")
	ErrNotFound           = errors.New("owners: not found")
	ErrSessionExpired     = errors.New("owners: session expired or invalid")
)

func collection() *mongo.Collection         { return database.GetDB().Collection("owners") }
func sessionsCollection() *mongo.Collection { return database.GetDB().Collection("owner_sessions") }

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

func Register(ctx context.Context, email, password, fullName string) (*Owner, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	o := &Owner{Email: email, PasswordHash: string(hash), FullName: fullName, CreatedAt: time.Now().UTC()}
	res, err := collection().InsertOne(ctx, o)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil, ErrEmailTaken
		}
		return nil, err
	}
	o.ID = res.InsertedID.(primitive.ObjectID)
	return o, nil
}

func GetByEmail(ctx context.Context, email string) (*Owner, error) {
	var o Owner
	err := collection().FindOne(ctx, bson.M{"email": email}).Decode(&o)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	return &o, err
}

func GetByID(ctx context.Context, id primitive.ObjectID) (*Owner, error) {
	var o Owner
	err := collection().FindOne(ctx, bson.M{"_id": id}).Decode(&o)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	return &o, err
}

func Login(ctx context.Context, email, password string) (*Owner, string, error) {
	o, err := GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, "", ErrInvalidCredentials
		}
		return nil, "", err
	}
	if bcrypt.CompareHashAndPassword([]byte(o.PasswordHash), []byte(password)) != nil {
		return nil, "", ErrInvalidCredentials
	}
	token, err := createSession(ctx, o.ID)
	if err != nil {
		return nil, "", err
	}
	return o, token, nil
}

func createSession(ctx context.Context, ownerID primitive.ObjectID) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	now := time.Now().UTC()
	_, err := sessionsCollection().InsertOne(ctx, Session{
		Token: token, OwnerID: ownerID, ExpiresAt: now.Add(sessionTTL), CreatedAt: now,
	})
	return token, err
}

// ValidateSession returns the owner a session token belongs to, or
// ErrSessionExpired if it's missing/expired (the TTL index also reaps
// expired sessions server-side, but a request can race that).
func ValidateSession(ctx context.Context, token string) (*Owner, error) {
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
	return GetByID(ctx, sess.OwnerID)
}

func Logout(ctx context.Context, token string) error {
	_, err := sessionsCollection().DeleteOne(ctx, bson.M{"_id": token})
	return err
}
