// Copyright 2021 Ahmet Alp Balkan
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package userdb

import (
	"context"
	"fmt"
	"time"

	firestore "cloud.google.com/go/firestore"
	grpc_auth "github.com/grpc-ecosystem/go-grpc-middleware/auth"
	"github.com/grpcoin/grpcoin/api/grpcoin"
	"github.com/grpcoin/grpcoin/server/auth"
	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ctxUserRecordKey struct{}

const (
	fsUserCol      = "users"      // users collection
	fsOrdersCol    = "orders"     // sub-collection for user
	fsValueHistCol = "valuations" // sub-collection for user's portolio value over time
)

type User struct {
	ID          string
	DisplayName string
	ProfileURL  string
	CreatedAt   time.Time
	Portfolio   Portfolio
}

type Portfolio struct {
	CashUSD   Amount
	Positions map[string]Amount
}

type Amount struct {
	Units int64
	Nanos int32
}

type Order struct {
	Date   time.Time           `firestore:"date"`
	Ticker string              `firestore:"ticker"`
	Action grpcoin.TradeAction `firestore:"action"`
	Size   Amount              `firestore:"size"`
	Price  Amount              `firestore:"price"`
}

type ValuationHistory struct {
	Date  time.Time `firestore:"date"`
	Value Amount    `firestore:"value"`
}

/*
	- user
	    - MAIN RECORD
		  - id
		  - signup date
		  - portfolio {cash, coin pos}
		  - valuation summary {1h:?? 24h:?? 7d:?? 30d:??}
		- valuations
		  - t1: v1
		  - t2: v1
		- /user/1/orders
*/

func (a Amount) V() *grpcoin.Amount { return &grpcoin.Amount{Units: a.Units, Nanos: a.Nanos} }
func (a Amount) F() decimal.Decimal { return toDecimal(a.V()) }

type UserDB struct {
	DB *firestore.Client
	T  trace.Tracer
}

func (u *UserDB) Create(ctx context.Context, au auth.AuthenticatedUser) error {
	newUser := User{ID: au.DBKey(),
		DisplayName: au.DisplayName(),
		ProfileURL:  au.ProfileURL(),
		CreatedAt:   time.Now(),
	}
	setupGamePortfolio(&newUser)
	// TODO: add an initial portfolio value to user's valuation history
	_, err := u.DB.Collection(fsUserCol).Doc(au.DBKey()).Create(ctx, newUser)
	return err
}

func (u *UserDB) Get(ctx context.Context, userID string) (User, bool, error) {
	doc, err := u.DB.Collection(fsUserCol).Doc(userID).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return User{}, false, nil
		}
		return User{}, false, status.Errorf(codes.Internal, "failed to retrieve user: %v", err)
	}
	var uv User
	if err := doc.DataTo(&uv); err != nil {
		return User{}, false, fmt.Errorf("failed to unpack user record %q: %w", userID, err)
	}
	return uv, true, nil
}

func (u *UserDB) GetAll(ctx context.Context) ([]User, error) {
	var out []User
	iter := u.DB.Collection(fsUserCol).Documents(ctx)
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		var u User
		if err := doc.DataTo(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

func (u *UserDB) EnsureAccountExists(ctx context.Context, au auth.AuthenticatedUser) (User, error) {
	ctx, s := u.T.Start(ctx, "ensure user account")
	defer s.End()
	user, exists, err := u.Get(ctx, au.DBKey())
	if exists {
		return user, nil
	} else if err != nil {
		return User{}, status.Errorf(codes.Internal, "failed to query user: %v", err)
	}
	err = u.Create(ctx, au)
	if err != nil {
		return User{}, status.Errorf(codes.Internal, "failed to create user: %v", err)
	}
	user, _, err = u.Get(ctx, au.DBKey())
	if err != nil {
		return User{}, status.Errorf(codes.Internal, "failed to query new user: %v", err)
	}
	return user, err
}

// EnsureAccountExistsInterceptor creates an account for the authenticated
// client (or retrieves it) and augments the ctx with the user's db record.
func (u *UserDB) EnsureAccountExistsInterceptor() grpc_auth.AuthFunc {
	return func(ctx context.Context) (context.Context, error) {
		authenticatedUser := auth.AuthInfoFromContext(ctx)
		if authenticatedUser == nil {
			return ctx, status.Error(codes.Internal, "req ctx did not have user info")
		}
		v, ok := authenticatedUser.(auth.AuthenticatedUser)
		if !ok {
			return ctx, status.Errorf(codes.Internal, "unknown authed user type %T", authenticatedUser)
		}
		uv, err := u.EnsureAccountExists(ctx, v)
		if err != nil {
			return ctx, status.Errorf(codes.Internal, "failed to ensure user account: %v", err)
		}
		return WithUserRecord(ctx, uv), nil
	}
}

func (u *UserDB) Trade(ctx context.Context, uid string, ticker string, action grpcoin.TradeAction, quote, quantity *grpcoin.Amount) error {
	subCtx, s := u.T.Start(ctx, "trade tx")
	ref := u.DB.Collection("users").Doc(uid)
	err := u.DB.RunTransaction(subCtx, func(ctx context.Context, tx *firestore.Transaction) error {
		doc, err := tx.Get(ref)
		if err != nil {
			return fmt.Errorf("failed to read user record for tx: %w", err)
		}
		var u User
		if err := doc.DataTo(&u); err != nil {
			return fmt.Errorf("failed to unpack user record into struct: %w", err)
		}
		if err := makeTrade(&u.Portfolio, action, ticker, quote, quantity); err != nil {
			return err
		}
		return tx.Set(ref, u)
	}, firestore.MaxAttempts(1))
	s.End()

	if err != nil {
		return err
	}

	subCtx, s = u.T.Start(ctx, "log order")
	defer s.End()
	t := time.Now().UTC()
	id := t.Format(time.RFC3339Nano)
	_, err = u.DB.Collection(fsUserCol).Doc(uid).Collection(fsOrdersCol).Doc(id).Create(ctx, Order{
		Date:   t,
		Ticker: ticker,
		Action: action,
		Size:   ToAmount(toDecimal(quantity)),
		Price:  ToAmount(toDecimal(quote)),
	})
	return nil
}

func (u *UserDB) UserOrderHistory(ctx context.Context, uid string) ([]Order, error) {
	ctx, s := u.T.Start(ctx, "order history")
	defer s.End()
	var out []Order
	iter := u.DB.Collection(fsUserCol).Doc(uid).Collection(fsOrdersCol).Documents(ctx)
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			s.RecordError(err)
			return nil, err
		}
		var v Order
		if err := doc.DataTo(&v); err != nil {
			s.RecordError(err)
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (u *UserDB) UserValuationHistory(ctx context.Context, uid string) ([]ValuationHistory, error) {
	ctx, s := u.T.Start(ctx, "user valuation history")
	defer s.End()
	var out []ValuationHistory
	iter := u.DB.Collection(fsUserCol).Doc(uid).Collection(fsValueHistCol).Documents(ctx)
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			s.RecordError(err)
			return nil, err
		}
		var v ValuationHistory
		if err := doc.DataTo(&v); err != nil {
			s.RecordError(err)
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func canonicalizeValuationHistoryDBKey(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func (u *UserDB) SetUserValuationHistory(ctx context.Context, uid string, v ValuationHistory) error {
	_, err := u.DB.Collection(fsUserCol).Doc(uid).Collection(fsValueHistCol).
		Doc(canonicalizeValuationHistoryDBKey(v.Date)).Create(ctx, v)
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return err
	}
	return nil
}

func (u *UserDB) RotateUserValuationHistory(ctx context.Context, uid string, deleteBefore time.Time) error {
	// TODO create an index for users/*/date ASC
	it := u.DB.Collection(fsUserCol).Doc(uid).Collection(fsValueHistCol).
		Where("date", "<", deleteBefore).Documents(ctx)
	wb := u.DB.Batch()
	var deleted int
	for {
		doc, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return err
		}
		wb.Delete(doc.Ref)
		deleted++
	}
	if deleted == 0 {
		return nil
	}
	_, err := wb.Commit(ctx)
	return err
}

func UserRecordFromContext(ctx context.Context) (User, bool) {
	v := ctx.Value(ctxUserRecordKey{})
	vv, ok := v.(User)
	return vv, ok
}

func WithUserRecord(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxUserRecordKey{}, u)
}
