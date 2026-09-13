// Package kyc is a deliberately minimal investor-verification gate — NOT a
// real identity-verification integration. Securitize (and app-core-backend,
// which already has a genuine Sumsub integration under internal/kyc)
// verify identity documents against a real provider; this package only
// tracks a status an admin sets by hand after doing that some other way.
// It exists so the investment flow has a real, enforced checkpoint to gate
// on — swapping in a real provider later means replacing Decide's caller,
// not the investment-flow code that checks Status.
package kyc

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"yeshara.network/marketplace/internal/database"
)

type Status string

const (
	StatusUnverified Status = "unverified"
	StatusPending    Status = "pending"
	StatusVerified   Status = "verified"
	StatusRejected   Status = "rejected"
)

type Record struct {
	InvestorID      primitive.ObjectID `bson:"_id" json:"investorId"`
	Status          Status             `bson:"status" json:"status"`
	FullName        string             `bson:"full_name" json:"fullName"`
	Country         string             `bson:"country" json:"country"`
	Address         string             `bson:"address,omitempty" json:"address,omitempty"`
	IDNumber        string             `bson:"id_number" json:"idNumber"`
	DateOfBirth     string             `bson:"date_of_birth" json:"dateOfBirth"`
	RejectionReason string             `bson:"rejection_reason,omitempty" json:"rejectionReason,omitempty"`
	SubmittedAt     time.Time          `bson:"submitted_at" json:"submittedAt"`
	DecidedAt       *time.Time         `bson:"decided_at,omitempty" json:"decidedAt,omitempty"`
	DecidedBy       string             `bson:"decided_by,omitempty" json:"decidedBy,omitempty"`
}

var ErrNotSubmitted = errors.New("kyc: not submitted")

func collection() *mongo.Collection { return database.GetDB().Collection("kyc_records") }

// Submit records an investor's self-declared identity info and moves them
// to pending — there is no verification step here, only the admin Decide
// call below actually changes the enforced Status.
func Submit(ctx context.Context, investorID primitive.ObjectID, fullName, country, address, idNumber, dob string) (*Record, error) {
	now := time.Now().UTC()
	rec := Record{
		InvestorID: investorID, Status: StatusPending,
		FullName: fullName, Country: country, Address: address, IDNumber: idNumber, DateOfBirth: dob,
		SubmittedAt: now,
	}
	_, err := collection().UpdateOne(ctx,
		bson.M{"_id": investorID},
		bson.M{"$set": rec},
		options.Update().SetUpsert(true),
	)
	return &rec, err
}

// UpdateAddress sets an investor's residential address without touching
// their verification Status — unlike Submit, this doesn't move a verified
// investor back to pending, so it's safe to call at any point after
// verification (e.g. filling in an address that predates this field, or a
// simple change of address).
func UpdateAddress(ctx context.Context, investorID primitive.ObjectID, address string) error {
	_, err := collection().UpdateOne(ctx,
		bson.M{"_id": investorID},
		bson.M{"$set": bson.M{"address": address}, "$setOnInsert": bson.M{"status": StatusUnverified}},
		options.Update().SetUpsert(true),
	)
	return err
}

func GetStatus(ctx context.Context, investorID primitive.ObjectID) (Status, error) {
	var rec Record
	err := collection().FindOne(ctx, bson.M{"_id": investorID}).Decode(&rec)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return StatusUnverified, nil
	}
	if err != nil {
		return "", err
	}
	return rec.Status, nil
}

func Get(ctx context.Context, investorID primitive.ObjectID) (*Record, error) {
	var rec Record
	err := collection().FindOne(ctx, bson.M{"_id": investorID}).Decode(&rec)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotSubmitted
	}
	return &rec, err
}

// Decide is the one place Status actually changes to verified/rejected —
// an admin action standing in for a real KYC provider's callback.
func Decide(ctx context.Context, investorID primitive.ObjectID, approved bool, reason, decidedBy string) error {
	status := StatusVerified
	if !approved {
		status = StatusRejected
	}
	now := time.Now().UTC()
	res, err := collection().UpdateOne(ctx,
		bson.M{"_id": investorID},
		bson.M{"$set": bson.M{
			"status": status, "rejection_reason": reason, "decided_at": now, "decided_by": decidedBy,
		}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return ErrNotSubmitted
	}
	return nil
}

func ListPending(ctx context.Context) ([]Record, error) {
	cur, err := collection().Find(ctx, bson.M{"status": StatusPending})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []Record{}
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
