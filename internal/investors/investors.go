// Package investors is Yeshara's own investor accounts — email/password
// login and sessions. Deliberately separate from comet-engine's tenant
// concept: an investor is an end user of Yeshara, not a comet-engine
// tenant. Each investor's Mongo ID doubles as their comet-engine
// externalUserId, so their custodial wallet identity is stable and simple
// to derive everywhere else in this codebase.
package investors

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

type Investor struct {
	ID           primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Email        string             `bson:"email" json:"email"`
	PasswordHash string             `bson:"password_hash" json:"-"`
	FullName     string             `bson:"full_name" json:"fullName"`
	CreatedAt    time.Time          `bson:"created_at" json:"createdAt"`

	// PayoutPhone is where yield distributions and redemption cash-outs are
	// sent when the mpesa_b2c rail is used — set once via the profile page
	// rather than collected per-payout, since a distribution run pays many
	// investors in one batch with no chance to prompt each of them.
	PayoutPhone string `bson:"payout_phone,omitempty" json:"payoutPhone,omitempty"`
}

// ExternalUserID is what this investor is called in comet-engine — their
// own Mongo ID, stable for the life of the account.
func (i *Investor) ExternalUserID() string {
	return "investor-" + i.ID.Hex()
}

type Session struct {
	Token      string             `bson:"_id"`
	InvestorID primitive.ObjectID `bson:"investor_id"`
	ExpiresAt  time.Time          `bson:"expires_at"`
	CreatedAt  time.Time          `bson:"created_at"`
}

const sessionTTL = 30 * 24 * time.Hour

var (
	ErrEmailTaken         = errors.New("investors: an account with this email already exists")
	ErrInvalidCredentials = errors.New("investors: invalid email or password")
	ErrNotFound           = errors.New("investors: not found")
	ErrSessionExpired     = errors.New("investors: session expired or invalid")
)

func collection() *mongo.Collection         { return database.GetDB().Collection("investors") }
func sessionsCollection() *mongo.Collection { return database.GetDB().Collection("investor_sessions") }

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

func Register(ctx context.Context, email, password, fullName string) (*Investor, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	inv := &Investor{Email: email, PasswordHash: string(hash), FullName: fullName, CreatedAt: time.Now().UTC()}
	res, err := collection().InsertOne(ctx, inv)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil, ErrEmailTaken
		}
		return nil, err
	}
	inv.ID = res.InsertedID.(primitive.ObjectID)
	return inv, nil
}

func GetByEmail(ctx context.Context, email string) (*Investor, error) {
	var inv Investor
	err := collection().FindOne(ctx, bson.M{"email": email}).Decode(&inv)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	return &inv, err
}

func GetByID(ctx context.Context, id primitive.ObjectID) (*Investor, error) {
	var inv Investor
	err := collection().FindOne(ctx, bson.M{"_id": id}).Decode(&inv)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	return &inv, err
}

func Login(ctx context.Context, email, password string) (*Investor, string, error) {
	inv, err := GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, "", ErrInvalidCredentials
		}
		return nil, "", err
	}
	if bcrypt.CompareHashAndPassword([]byte(inv.PasswordHash), []byte(password)) != nil {
		return nil, "", ErrInvalidCredentials
	}
	token, err := createSession(ctx, inv.ID)
	if err != nil {
		return nil, "", err
	}
	return inv, token, nil
}

func createSession(ctx context.Context, investorID primitive.ObjectID) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	now := time.Now().UTC()
	_, err := sessionsCollection().InsertOne(ctx, Session{
		Token: token, InvestorID: investorID, ExpiresAt: now.Add(sessionTTL), CreatedAt: now,
	})
	return token, err
}

// ValidateSession returns the investor a session token belongs to, or
// ErrSessionExpired if it's missing/expired (the TTL index also reaps
// expired sessions server-side, but a request can race that).
func ValidateSession(ctx context.Context, token string) (*Investor, error) {
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
	return GetByID(ctx, sess.InvestorID)
}

func Logout(ctx context.Context, token string) error {
	_, err := sessionsCollection().DeleteOne(ctx, bson.M{"_id": token})
	return err
}

// SetPayoutPhone records where this investor wants yield/redemption cash
// sent when paid via the mpesa_b2c rail.
func SetPayoutPhone(ctx context.Context, id primitive.ObjectID, phone string) error {
	_, err := collection().UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"payout_phone": phone}})
	return err
}
