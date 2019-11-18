// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package market

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/decred/dcrdex/dex"
	"github.com/decred/dcrdex/dex/msgjson"
	"github.com/decred/dcrdex/dex/order"
	"github.com/decred/dcrdex/server/account"
	"github.com/decred/dcrdex/server/asset"
	"github.com/decred/dcrdex/server/matcher"
)

const maxClockOffset = 10 // seconds

// The AuthManager handles client-related actions, including authorization and
// communications.
type AuthManager interface {
	Route(route string, handler func(account.AccountID, *msgjson.Message) *msgjson.Error)
	Auth(user account.AccountID, msg, sig []byte) error
	Sign(...msgjson.Signable)
	Send(account.AccountID, *msgjson.Message)
}

// MarketTunnel is a connection to a market and information about existing
// swaps.
type MarketTunnel interface {
	// SubmitOrderAsync submits the order to the market for insertion into the
	// epoch queue.
	SubmitOrderAsync(*orderRecord)
	// MidGap returns the mid-gap market rate, which is ths rate halfway between
	// the best buy order and the best sell order in the order book.
	MidGap() uint64
	// OutpointLocked should return true if the outpoint is currently a funding
	// UTXO for an active DEX order. This is required for UTXO validation to
	// prevent a user from submitting multiple orders spending the same UTXO. This
	// method will likely need to check all orders currently in the epoch queue,
	// the order book, and the swap monitor, since UTXOs will still be unspent
	// according to the asset backends until the client broadcasts their
	// initialization transaction.
	// DRAFT NOTE: This function could also potentially be handled by persistent
	// storage, since active orders and active matches are tracked there.
	OutpointLocked(txid string, vout uint32) bool
	// Cancelable determines whether an order is cancelable. A cancelable order
	// is a limit order with time-in-force standing either in the epoch queue or
	// in the order book.
	Cancelable(order.OrderID) bool
	// TxMonitored determines whether the transaction for the given user is
	// involved in a DEX-monitored trade. Change outputs from DEX-monitored trades
	// can be used in other orders without waiting for fundConf confirmations.
	TxMonitored(user account.AccountID, txid string) bool
}

// orderRecord contains the information necessary to respond to an order
// request.
type orderRecord struct {
	order order.Order
	req   msgjson.Stampable
	msgID uint64
}

// newOrderRecord is the constructor for an *orderRecord.
func newOrderRecord(o order.Order, req msgjson.Stampable, msgID uint64) *orderRecord {
	return &orderRecord{
		order: o,
		req:   req,
		msgID: msgID,
	}
}

// assetSet is pointers to two different assets, but with 4 ways of addressing
// them.
type assetSet struct {
	funding   *asset.Asset
	receiving *asset.Asset
	base      *asset.Asset
	quote     *asset.Asset
}

// newAssetSet is a constructor for an assetSet.
func newAssetSet(base, quote *asset.Asset, sell bool) *assetSet {
	coins := &assetSet{
		quote:     quote,
		base:      base,
		funding:   quote,
		receiving: base,
	}
	if sell {
		coins.funding, coins.receiving = base, quote
	}
	return coins
}

// outpoint satisfies order.Outpoint.
type outpoint struct {
	hash []byte
	vout uint32
}

// newOutpoint is a constructor for an outpoint.
func newOutpoint(h []byte, v uint32) *outpoint {
	return &outpoint{
		hash: h,
		vout: v,
	}
}

// Txhash is a getter for the outpoint's hash.
func (o *outpoint) TxHash() []byte { return o.hash }

// Vout is a getter for the outpoint's vout.
func (o *outpoint) Vout() uint32 { return o.vout }

// OrderRouter handles the 'limit', 'market', and 'cancel' DEX routes. These
// are authenticated routes used for placing and canceling orders.
type OrderRouter struct {
	auth     AuthManager
	assets   map[uint32]*asset.Asset
	tunnels  map[string]MarketTunnel
	mbBuffer float64
}

// OrderRouterConfig is the configuration settings for an OrderRouter.
type OrderRouterConfig struct {
	AuthManager     AuthManager
	Assets          map[uint32]*asset.Asset
	Markets         map[string]MarketTunnel
	MarketBuyBuffer float64
}

// NewOrderRouter is a constructor for an OrderRouter.
func NewOrderRouter(cfg *OrderRouterConfig) *OrderRouter {
	router := &OrderRouter{
		auth:     cfg.AuthManager,
		assets:   cfg.Assets,
		tunnels:  cfg.Markets,
		mbBuffer: cfg.MarketBuyBuffer,
	}
	cfg.AuthManager.Route(msgjson.LimitRoute, router.handleLimit)
	cfg.AuthManager.Route(msgjson.MarketRoute, router.handleMarket)
	cfg.AuthManager.Route(msgjson.CancelRoute, router.handleCancel)
	return router
}

// handleLimit is the handler for the 'limit' route. This route accepts a
// msgjson.Limit payload, validates the information, constructs an
// order.LimitOrder and submits it to the epoch queue.
func (r *OrderRouter) handleLimit(user account.AccountID, msg *msgjson.Message) *msgjson.Error {
	limit := new(msgjson.Limit)
	err := json.Unmarshal(msg.Payload, limit)
	if err != nil {
		return msgjson.NewError(msgjson.RPCParseError, "error decoding 'limit' payload")
	}

	rpcErr := r.verifyAccount(user, limit.AccountID, limit)
	if rpcErr != nil {
		return rpcErr
	}

	tunnel, coins, sell, rpcErr := r.extractMarketDetails(&limit.Prefix, &limit.Trade)
	if rpcErr != nil {
		return rpcErr
	}

	// Check that OrderType is set correctly
	if limit.OrderType != msgjson.LimitOrderNum {
		return msgjson.NewError(msgjson.OrderParameterError, "wrong order type set for limit order")
	}

	valSum, spendSize, utxos, rpcErr := r.checkPrefixTrade(user, tunnel, coins, &limit.Prefix, &limit.Trade, true)
	if rpcErr != nil {
		return rpcErr
	}

	// Check that the rate is non-zero and obeys the rate step interval.
	if limit.Rate == 0 {
		return msgjson.NewError(msgjson.OrderParameterError, "rate = 0 not allowed")
	}
	if limit.Rate%coins.quote.RateStep != 0 {
		return msgjson.NewError(msgjson.OrderParameterError, "rate not a multiple of ratestep")
	}

	// Calculate the fees and check that the utxo sum is enough.
	swapVal := limit.Quantity
	if !sell {
		swapVal = matcher.BaseToQuote(limit.Rate, limit.Quantity)
	}
	reqVal := requiredFunds(swapVal, spendSize, coins.funding)
	if valSum < reqVal {
		return msgjson.NewError(msgjson.FundingError,
			fmt.Sprintf("not enough funds. need at least %d, got %d", reqVal, valSum))
	}

	// Check time-in-force
	if !(limit.TiF == msgjson.StandingOrderNum || limit.TiF == msgjson.ImmediateOrderNum) {
		return msgjson.NewError(msgjson.OrderParameterError, "unknown time-in-force")
	}

	// Create the limit order
	serverTime := time.Now().UTC()
	lo := &order.LimitOrder{
		MarketOrder: order.MarketOrder{
			Prefix: order.Prefix{
				AccountID:  user,
				BaseAsset:  limit.Base,
				QuoteAsset: limit.Quote,
				OrderType:  order.LimitOrderType,
				ClientTime: time.Unix(int64(limit.ClientTime), 0).UTC(),
				ServerTime: serverTime,
			},
			UTXOs:    utxos,
			Sell:     sell,
			Quantity: limit.Quantity,
			Address:  limit.Address,
		},
		Rate:  limit.Rate,
		Force: order.StandingTiF,
	}

	// Send the order to the epoch queue.
	oRecord := &orderRecord{
		order: lo,
		req:   limit,
		msgID: msg.ID,
	}
	tunnel.SubmitOrderAsync(oRecord)
	r.respondOrder(oRecord)
	return nil
}

// handleMarket is the handler for the 'market' route. This route accepts a
// msgjson.Market payload, validates the information, constructs an
// order.MarketOrder and submits it to the epoch queue.
func (r *OrderRouter) handleMarket(user account.AccountID, msg *msgjson.Message) *msgjson.Error {
	market := new(msgjson.Market)
	err := json.Unmarshal(msg.Payload, market)
	if err != nil {
		return msgjson.NewError(msgjson.RPCParseError, "error decoding 'market' payload")
	}

	rpcErr := r.verifyAccount(user, market.AccountID, market)
	if rpcErr != nil {
		return rpcErr
	}

	tunnel, coins, sell, rpcErr := r.extractMarketDetails(&market.Prefix, &market.Trade)
	if rpcErr != nil {
		return rpcErr
	}

	// Check that OrderType is set correctly
	if market.OrderType != msgjson.MarketOrderNum {
		return msgjson.NewError(msgjson.OrderParameterError, "wrong order type set for market order")
	}

	// Passing sell as the checkLot parameter causes the lot size check to be
	// ignored for market buy orders.
	valSum, spendSize, utxos, rpcErr := r.checkPrefixTrade(user, tunnel, coins, &market.Prefix, &market.Trade, sell)
	if rpcErr != nil {
		return rpcErr
	}

	// Calculate the fees and check that the utxo sum is enough.
	var reqVal uint64
	if sell {
		reqVal = requiredFunds(market.Quantity, spendSize, coins.funding)
	} else {
		// This is a market buy order, so the quantity gets special handling.
		// 1. The quantity is in units of the quote asset.
		// 2. The quantity has to satisfy the market buy buffer.
		reqVal = matcher.QuoteToBase(tunnel.MidGap(), market.Quantity)
		lotWithBuffer := uint64(float64(coins.base.LotSize) * r.mbBuffer)
		minReq := matcher.QuoteToBase(tunnel.MidGap(), lotWithBuffer)
		if reqVal < minReq {
			return msgjson.NewError(msgjson.FundingError, "order quantity does not satisfy market buy buffer")
		}
	}
	if valSum < reqVal {
		return msgjson.NewError(msgjson.FundingError,
			fmt.Sprintf("not enough funds. need at least %d, got %d", reqVal, valSum))
	}
	// Create the market order
	serverTime := time.Now().UTC()
	mo := &order.MarketOrder{
		Prefix: order.Prefix{
			AccountID:  user,
			BaseAsset:  market.Base,
			QuoteAsset: market.Quote,
			OrderType:  order.MarketOrderType,
			ClientTime: time.Unix(int64(market.ClientTime), 0).UTC(),
			ServerTime: serverTime,
		},
		UTXOs:    utxos,
		Sell:     sell,
		Quantity: market.Quantity,
		Address:  market.Address,
	}

	// Send the order to the epoch queue.
	oRecord := &orderRecord{
		order: mo,
		req:   market,
		msgID: msg.ID,
	}
	tunnel.SubmitOrderAsync(oRecord)
	r.respondOrder(oRecord)
	return nil
}

// handleCancel is the handler for the 'cancel' route. This route accepts a
// msgjson.Cancel payload, validates the information, constructs an
// order.CancelOrder and submits it to the epoch queue.
func (r *OrderRouter) handleCancel(user account.AccountID, msg *msgjson.Message) *msgjson.Error {
	cancel := new(msgjson.Cancel)
	err := json.Unmarshal(msg.Payload, cancel)
	if err != nil {
		return msgjson.NewError(msgjson.RPCParseError, "error decoding 'cancel' payload")
	}

	rpcErr := r.verifyAccount(user, cancel.AccountID, cancel)
	if rpcErr != nil {
		return rpcErr
	}

	tunnel, rpcErr := r.extractMarket(&cancel.Prefix)
	if rpcErr != nil {
		return rpcErr
	}

	if len(cancel.TargetID) != order.OrderIDSize {
		return msgjson.NewError(msgjson.OrderParameterError, "invalid target ID format")
	}
	var targetID order.OrderID
	copy(targetID[:], cancel.TargetID)

	if !tunnel.Cancelable(targetID) {
		return msgjson.NewError(msgjson.OrderParameterError, "target order not known")
	}

	// Check that OrderType is set correctly
	if cancel.OrderType != msgjson.CancelOrderNum {
		return msgjson.NewError(msgjson.OrderParameterError, "wrong order type set for cancel order")
	}

	rpcErr = checkTimes(&cancel.Prefix)
	if rpcErr != nil {
		return rpcErr
	}

	// Create the cancel order
	serverTime := time.Now().UTC()
	co := &order.CancelOrder{
		Prefix: order.Prefix{
			AccountID:  user,
			BaseAsset:  cancel.Base,
			QuoteAsset: cancel.Quote,
			OrderType:  order.MarketOrderType,
			ClientTime: time.Unix(int64(cancel.ClientTime), 0).UTC(),
			ServerTime: serverTime,
		},
		TargetOrderID: targetID,
	}

	// Send the order to the epoch queue.
	oRecord := &orderRecord{
		order: co,
		req:   cancel,
		msgID: msg.ID,
	}
	tunnel.SubmitOrderAsync(oRecord)
	r.respondOrder(oRecord)
	return nil
}

// verifyAccount checks that the submitted order squares with the submitting user.
func (r *OrderRouter) verifyAccount(user account.AccountID, msgAcct msgjson.Bytes, signable msgjson.Signable) *msgjson.Error {
	// Verify account ID matches.
	if !bytes.Equal(user[:], msgAcct) {
		return msgjson.NewError(msgjson.OrderParameterError, "account ID mismatch")
	}
	// Check the clients signature of the order.
	// DRAFT NOTE: These Serialize methods actually never return errors. We should
	// just drop the error return value.
	sigMsg, _ := signable.Serialize()
	err := r.auth.Auth(user, sigMsg, signable.SigBytes())
	if err != nil {
		return msgjson.NewError(msgjson.SignatureError, "signature error: "+err.Error())
	}
	return nil
}

// extractMarket finds the MarketTunnel for the provided prefix.
func (r *OrderRouter) extractMarket(prefix *msgjson.Prefix) (MarketTunnel, *msgjson.Error) {
	mktName, err := dex.MarketName(prefix.Base, prefix.Quote)
	if err != nil {
		return nil, msgjson.NewError(msgjson.UnknownMarketError, "asset lookup error: "+err.Error())
	}
	tunnel, found := r.tunnels[mktName]
	if !found {
		return nil, msgjson.NewError(msgjson.UnknownMarketError, "unknown market "+mktName)
	}
	return tunnel, nil
}

// extractMarketDetails finds the MarketTunnel, side, and an assetSet for the
// provided prefix.
func (r *OrderRouter) extractMarketDetails(prefix *msgjson.Prefix, trade *msgjson.Trade) (MarketTunnel, *assetSet, bool, *msgjson.Error) {
	// Check that assets are for a valid market.
	tunnel, rpcErr := r.extractMarket(prefix)
	if rpcErr != nil {
		return nil, nil, false, rpcErr
	}
	// Side must be one of buy or sell
	var sell bool
	switch trade.Side {
	case msgjson.BuyOrderNum:
	case msgjson.SellOrderNum:
		sell = true
	default:
		return nil, nil, false, msgjson.NewError(msgjson.OrderParameterError,
			fmt.Sprintf("invalid side value %d", trade.Side))
	}
	quote, found := r.assets[prefix.Quote]
	if !found {
		panic("missing quote asset for known market should be impossible")
	}
	base, found := r.assets[prefix.Base]
	if !found {
		panic("missing base asset for known market should be impossible")
	}
	return tunnel, newAssetSet(base, quote, sell), sell, nil
}

// checkTimes validates the timestamps in an order prefix.
func checkTimes(prefix *msgjson.Prefix) *msgjson.Error {
	offset := time.Now().Unix() - int64(prefix.ClientTime)
	if offset < 0 {
		offset *= -1
	}
	if offset >= maxClockOffset {
		return msgjson.NewError(msgjson.ClockRangeError, fmt.Sprintf(
			"clock offset of %d seconds is larger than maximum allowed, %d seconds",
			offset, maxClockOffset,
		))
	}
	// Server time should be unset.
	if prefix.ServerTime != 0 {
		return msgjson.NewError(msgjson.OrderParameterError, "non-zero server time not allowed")
	}
	return nil
}

// checkPrefixTrade validates the information in the prefix and trade portions
// of an order.
func (r *OrderRouter) checkPrefixTrade(user account.AccountID, tunnel MarketTunnel, coins *assetSet, prefix *msgjson.Prefix,
	trade *msgjson.Trade, checkLot bool) (uint64, uint32, []order.Outpoint, *msgjson.Error) {
	// Check that the client's timestamp is still valid.
	rpcErr := checkTimes(prefix)
	if rpcErr != nil {
		return 0, 0, nil, rpcErr
	}
	errSet := func(code int, message string) (uint64, uint32, []order.Outpoint, *msgjson.Error) {
		return 0, 0, nil, msgjson.NewError(code, message)
	}
	// Check that the address is valid.
	if !coins.receiving.Backend.CheckAddress(trade.Address) {
		return errSet(msgjson.OrderParameterError, "address doesn't check")
	}
	// Quantity cannot be zero, and must be an integral multiple of the lot size.
	if trade.Quantity == 0 {
		return errSet(msgjson.OrderParameterError, "zero quantity not allowed")
	}
	if checkLot && trade.Quantity%coins.base.LotSize != 0 {
		return errSet(msgjson.OrderParameterError, "order quantity not a multiple of lot size")
	}
	// Validate UTXOs
	// Check that all required arrays are of equal length.
	if len(trade.UTXOs) == 0 {
		return errSet(msgjson.FundingError, "order must specify utxos")
	}
	var valSum uint64
	var spendSize uint32
	var utxos []order.Outpoint
	for i, utxo := range trade.UTXOs {
		sigCount := len(utxo.Sigs)
		if sigCount == 0 {
			return errSet(msgjson.SignatureError, fmt.Sprintf("no signature for utxo %d", i))
		}
		if len(utxo.PubKeys) != sigCount {
			return errSet(msgjson.OrderParameterError, fmt.Sprintf(
				"pubkey count %d not equal to signature count %d for utxo %d",
				len(utxo.PubKeys), sigCount, i,
			))
		}
		txid := utxo.TxID.String()
		// Check that the outpoint isn't locked.
		locked := tunnel.OutpointLocked(txid, utxo.Vout)
		if locked {
			return errSet(msgjson.FundingError,
				fmt.Sprintf("utxo %s:%d is locked", utxo.TxID.String(), utxo.Vout))
		}
		// Get the utxo from the backend and validate it.
		dexUTXO, err := coins.funding.Backend.UTXO(txid, utxo.Vout, utxo.Redeem)
		if err != nil {
			return errSet(msgjson.FundingError,
				fmt.Sprintf("error retreiving utxo %s:%d", utxo.TxID.String(), utxo.Vout))
		}
		// Make sure the UTXO has the requisite number of confirmations.
		confs, err := dexUTXO.Confirmations()
		if err != nil {
			return errSet(msgjson.FundingError,
				fmt.Sprintf("utxo confirmations error for %s:%d: %v", utxo.TxID.String(), utxo.Vout, err))
		}
		if confs < int64(coins.funding.FundConf) && !tunnel.TxMonitored(user, txid) {
			return errSet(msgjson.FundingError,
				fmt.Sprintf("not enough confirmations for %s:%d. require %d, have %d",
					utxo.TxID.String(), utxo.Vout, coins.funding.FundConf, confs))
		}
		sigMsg := utxo.Serialize()
		err = dexUTXO.Auth(msgBytesToBytes(utxo.PubKeys), msgBytesToBytes(utxo.Sigs), sigMsg)
		if err != nil {
			return errSet(msgjson.UTXOAuthError,
				fmt.Sprintf("failed to authorize utxo %s:%d", utxo.TxID.String(), utxo.Vout))
		}
		utxos = append(utxos, newOutpoint(utxo.TxID, utxo.Vout))
		valSum += dexUTXO.Value()
		spendSize += dexUTXO.SpendSize()
	}
	return valSum, spendSize, utxos, nil
}

// requiredFunds calculates the minimum amount needed to fulfill the swap amount
// and pay transaction fees. The spendSize is the sum of the serialized inputs
// associated with a set of UTXOs to be spent. The swapVal is the total quantity
// needed to fulfill an order.
func requiredFunds(swapVal uint64, spendSize uint32, coin *asset.Asset) uint64 {
	R := float64(coin.SwapSize) * float64(coin.FeeRate) / float64(coin.LotSize)
	fBase := uint64(float64(swapVal) * R)
	fUtxo := uint64(spendSize) * coin.FeeRate
	return swapVal + fBase + fUtxo
}

// msgBytesToBytes converts a []msgjson.Byte to a [][]byte.
func msgBytesToBytes(msgBs []msgjson.Bytes) [][]byte {
	b := make([][]byte, 0, len(msgBs))
	for _, msgB := range msgBs {
		b = append(b, msgB)
	}
	return b
}

// respondOrder signs the order data and sends the OrderResult to the client.
func (r *OrderRouter) respondOrder(oRecord *orderRecord) {
	// Add the server timestamp and get a signature of the serialized
	// order request to send to the client.
	stamp := uint64(oRecord.order.Time())
	oRecord.req.Stamp(stamp)
	oid := oRecord.order.ID()
	r.auth.Sign(oRecord.req)
	res := &msgjson.OrderResult{
		Sig:        oRecord.req.SigBytes(),
		ServerTime: stamp,
		OrderID:    oid[:],
	}
	respMsg, err := msgjson.NewResponse(oRecord.msgID, res, nil)
	if err != nil {
		log.Errorf("failed to create msgjson.Message for order response: %v", err)
		return
	}
	r.auth.Send(oRecord.order.User(), respMsg)
}