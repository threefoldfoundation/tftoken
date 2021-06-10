package bridge

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/routing"
	"github.com/stellar/go/amount"
	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	hProtocol "github.com/stellar/go/protocols/horizon"
	horizoneffects "github.com/stellar/go/protocols/horizon/effects"
	"github.com/stellar/go/protocols/horizon/operations"
	"github.com/stellar/go/support/errors"
	"github.com/stellar/go/txnbuild"
)

const (
	TFTMainnet = "TFT:GBOVQKJYHXRR3DX6NOX2RRYFRCUMSADGDESTDNBDS6CDVLGVESRTAC47"
	TFTTest    = "TFT:GA47YZA3PKFUZMPLQ3B5F2E3CJIB57TGGU7SPCQT2WAEYKN766PWIMB3"

	stellarPrecision       = 1e7
	stellarPrecisionDigits = 7
)

// stellarWallet is the bridge wallet
// Payments will be funded and fees will be taken with this wallet
type stellarWallet struct {
	keypair                   *keypair.Full
	config                    *StellarConfig
	stellarTransactionStorage *StellarTransactionStorage
	signerWallet
}

type signerWallet struct {
	client         *SignersClient
	signatureCount int
}

func newStellarWallet(ctx context.Context, config *StellarConfig) (*stellarWallet, error) {
	kp, err := keypair.ParseFull(config.StellarSeed)

	if err != nil {
		return nil, err
	}

	stellarTransactionStorage := NewStellarTransactionStorage(config.StellarNetwork, kp.Address())
	w := &stellarWallet{
		keypair:                   kp,
		config:                    config,
		stellarTransactionStorage: stellarTransactionStorage,
	}

	return w, nil
}

func (w *stellarWallet) newSignerClient(ctx context.Context, host host.Host, router routing.PeerRouting) error {
	account, err := w.GetAccountDetails(w.keypair.Address())
	if err != nil {
		return err
	}
	var keys []string
	for _, signer := range account.Signers {
		if signer.Key == w.keypair.Address() {
			continue
		}
		keys = append(keys, signer.Key)
	}

	log.Info("required signature count", "signatures", int(account.Thresholds.MedThreshold))
	w.signatureCount = int(account.Thresholds.MedThreshold) - 1

	w.client, err = NewSignersClient(ctx, host, router, keys)
	if err != nil {
		return err
	}
	return nil
}

func (w *stellarWallet) CreateAndSubmitPayment(ctx context.Context, target string, amount uint64, receiver common.Address, blockheight uint64, txHash common.Hash, message string, includeWithdrawFee bool) error {
	txnBuild, err := w.generatePaymentOperation(amount, target, includeWithdrawFee)
	if err != nil {
		return err
	}

	txnBuild.Memo = txnbuild.MemoHash(txHash)

	signReq := SignRequest{
		RequiredSignatures: w.signatureCount,
		Receiver:           receiver,
		Block:              blockheight,
		Message:            message,
	}

	return w.submitTransaction(ctx, txnBuild, signReq)
}

func (w *stellarWallet) CreateAndSubmitRefund(ctx context.Context, target string, amount uint64, message string, includeWithdrawFee bool) error {
	txnBuild, err := w.generatePaymentOperation(amount, target, includeWithdrawFee)
	if err != nil {
		return err
	}

	parsedMessage, err := hex.DecodeString(message)
	if err != nil {
		return err
	}

	var memo [32]byte
	copy(memo[:], parsedMessage)

	txnBuild.Memo = txnbuild.MemoReturn(memo)

	signReq := SignRequest{
		RequiredSignatures: w.signatureCount,
		Message:            message,
	}

	return w.submitTransaction(ctx, txnBuild, signReq)
}

// CreateAndSubmitFeepayment creates and submites a payment to the fee wallet
// only an amount and hash needs to be specified
func (w *stellarWallet) CreateAndSubmitFeepayment(ctx context.Context, amount uint64, txHash common.Hash) error {
	feeWalletAddress := w.keypair.Address()
	if w.config.StellarFeeWallet != "" {
		feeWalletAddress = w.config.StellarFeeWallet
	}

	txnBuild, err := w.generatePaymentOperation(amount, feeWalletAddress, false)
	if err != nil {
		return errors.Wrap(err, "failed to generate payment operation")
	}

	txnBuild.Memo = txnbuild.MemoHash(txHash)

	signReq := SignRequest{
		RequiredSignatures: w.signatureCount,
	}

	return w.submitTransaction(ctx, txnBuild, signReq)
}

func (w *stellarWallet) generatePaymentOperation(amount uint64, destination string, includeWithdrawFee bool) (txnbuild.TransactionParams, error) {
	// if amount is zero, do nothing
	if amount == 0 {
		return txnbuild.TransactionParams{}, errors.New("invalid amount")
	}

	sourceAccount, err := w.GetAccountDetails(w.keypair.Address())
	if err != nil {
		return txnbuild.TransactionParams{}, errors.Wrap(err, "failed to get source account")
	}

	asset := w.GetAssetCodeAndIssuer()

	var paymentOperations []txnbuild.Operation
	paymentOP := txnbuild.Payment{
		Destination: destination,
		Amount:      big.NewRat(int64(amount), stellarPrecision).FloatString(stellarPrecisionDigits),
		Asset: txnbuild.CreditAsset{
			Code:   asset[0],
			Issuer: asset[1],
		},
		SourceAccount: sourceAccount.AccountID,
	}
	paymentOperations = append(paymentOperations, &paymentOP)

	if includeWithdrawFee {
		feePaymentOP := txnbuild.Payment{
			Destination: w.config.StellarFeeWallet,
			Amount:      big.NewRat(WithdrawFee, stellarPrecision).FloatString(stellarPrecisionDigits),
			Asset: txnbuild.CreditAsset{
				Code:   asset[0],
				Issuer: asset[1],
			},
			SourceAccount: sourceAccount.AccountID,
		}
		paymentOperations = append(paymentOperations, &feePaymentOP)
	}

	txnBuild := txnbuild.TransactionParams{
		Operations:           paymentOperations,
		Timebounds:           txnbuild.NewTimeout(300),
		SourceAccount:        &sourceAccount,
		BaseFee:              txnbuild.MinBaseFee * 3,
		IncrementSequenceNum: true,
	}

	return txnBuild, nil
}

func (w *stellarWallet) submitTransaction(ctx context.Context, txn txnbuild.TransactionParams, signReq SignRequest) error {
	tx, err := txnbuild.NewTransaction(txn)
	if err != nil {
		return errors.Wrap(err, "failed to build transaction")
	}

	// check if a similar transaction with a memo was made before
	exists, err := w.stellarTransactionStorage.TransactionWithMemoExists(tx)
	if err != nil {
		return errors.Wrap(err, "failed to check transaction storage for existing transaction hash")
	}
	// if the transaction exists, return with nil error
	if exists {
		log.Info("Transaction with this hash already executed, skipping now..")
		return nil
	}

	client, err := w.GetHorizonClient()
	if err != nil {
		return errors.Wrap(err, "failed to get horizon client")
	}

	// Only try to request signatures if there are signatures required
	if w.signatureCount > 0 {
		xdr, err := tx.Base64()
		if err != nil {
			return errors.Wrap(err, "failed to serialize transaction")
		}
		signReq.TxnXDR = xdr

		signatures, err := w.client.Sign(ctx, signReq)
		if err != nil {
			return err
		}

		if len(signatures) < w.signatureCount {
			return fmt.Errorf("received %d signatures, needed %d", len(signatures), w.signatureCount)
		}

		for _, signature := range signatures {
			tx, err = tx.AddSignatureBase64(w.GetNetworkPassPhrase(), signature.Address, signature.Signature)
			if err != nil {
				log.Error("Failed to add signature", "err", err.Error())
				return err
			}
		}
	}

	tx, err = tx.Sign(w.GetNetworkPassPhrase(), w.keypair)
	if err != nil {
		if hError, ok := err.(*horizonclient.Error); ok {
			log.Error("Error submitting tx", "extras", hError.Problem.Extras)
		}
		return errors.Wrap(err, "failed to sign transaction with keypair")
	}

	// Submit the transaction
	txResult, err := client.SubmitTransaction(tx)
	if err != nil {
		if hError, ok := err.(*horizonclient.Error); ok {
			log.Error("Error submitting tx", "extras", hError.Problem.Extras)
		}
		return errors.Wrap(err, "error submitting transaction")
	}
	log.Info(fmt.Sprintf("transaction: %s submitted to the stellar network..", txResult.Hash))

	w.stellarTransactionStorage.StoreTransactionWithMemo(tx)
	if err != nil {
		return errors.Wrap(err, "failed to store transaction with memo")
	}

	return nil
}

func (w *stellarWallet) refundTransaction(ctx context.Context, totalAmount uint64, tx hProtocol.Transaction) {
	ops, err := w.getOperationEffect(tx.Hash)
	if err != nil {
		return
	}
	for _, op := range ops.Embedded.Records {
		if op.GetType() == "payment" {
			paymentOperation := op.(operations.Payment)

			if paymentOperation.To == w.keypair.Address() {
				amount := totalAmount - uint64(WithdrawFee)
				if amount == 0 {
					return
				}
				log.Warn("Calling refund")

				err := w.CreateAndSubmitRefund(ctx, paymentOperation.From, amount, tx.Hash, true)
				for err != nil {
					log.Error("error while trying to refund user", "err", err.Error(), "amount", amount)
					select {
					case <-ctx.Done():
						return
					case <-time.After(10 * time.Second):
						err = w.CreateAndSubmitRefund(ctx, paymentOperation.From, amount, tx.Hash, true)
					}
				}
			}
		}
	}

}

// mint handler
type mint func(ERC20Address, *big.Int, string) error

//MonitorBridgeAccountAndMint is a blocking function that keeps monitoring
// the bridge account on the Stellar network for new transactions and calls the
// mint function when a deposit is made
func (w *stellarWallet) MonitorBridgeAccountAndMint(ctx context.Context, mintFn mint, persistency *ChainPersistency) error {
	transactionHandler := func(tx hProtocol.Transaction) {
		if !tx.Successful {
			return
		}
		log.Info("Received transaction on bridge stellar account", "hash", tx.Hash)

		effects, err := w.getTransactionEffects(tx.Hash)
		if err != nil {
			log.Error("error while fetching transaction effects:", err.Error())
			return
		}

		asset := w.GetAssetCodeAndIssuer()

		var totalAmount int64
		for _, effect := range effects.Embedded.Records {
			if effect.GetAccount() != w.keypair.Address() {
				continue
			}
			if effect.GetType() == "account_credited" {
				creditedEffect := effect.(horizoneffects.AccountCredited)
				if creditedEffect.Asset.Code != asset[0] && creditedEffect.Asset.Issuer != asset[1] {
					continue
				}
				parsedAmount, err := amount.ParseInt64(creditedEffect.Amount)
				if err != nil {
					continue
				}

				totalAmount += parsedAmount
			}
		}

		if totalAmount == 0 {
			return
		}
		log.Info("deposited amount", "a", totalAmount)
		depositedAmount := big.NewInt(totalAmount)

		data, err := base64.StdEncoding.DecodeString(tx.Memo)
		if err != nil {
			log.Error("error decoding transaction memo, calling refund", "error", err.Error())
			w.refundTransaction(ctx, uint64(totalAmount), tx)
			return
		}

		// if the user sent an invalid memo, return the funds
		if len(data) != 20 {
			log.Error("length of parsed memo is less than 20, caling refund")
			w.refundTransaction(ctx, uint64(totalAmount), tx)
			return
		}

		var ethAddress ERC20Address
		copy(ethAddress[0:20], data)

		err = mintFn(ethAddress, depositedAmount, tx.Hash)
		for err != nil {
			log.Error(fmt.Sprintf("Error occured while minting: %s", err.Error()))
			if err == errInsufficientDepositAmount {
				log.Warn("User is trying to swap less than the fee amount, refunding now", "amount", totalAmount)
				w.refundTransaction(ctx, uint64(totalAmount), tx)
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
				err = mintFn(ethAddress, depositedAmount, tx.Hash)
			}
		}

		if w.config.StellarFeeWallet != "" {
			log.Info("Trying to transfer the fees generated to the fee wallet", "address", w.config.StellarFeeWallet)

			// convert tx hash string to bytes
			parsedMessage, err := hex.DecodeString(tx.Hash)
			if err != nil {
				return
			}
			var memo [32]byte
			copy(memo[:], parsedMessage)

			err = w.CreateAndSubmitFeepayment(context.Background(), DepositFee, memo)
			for err != nil {
				log.Error("error while to send fees to the fee wallet", "err", err.Error())
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Second):
					err = w.CreateAndSubmitFeepayment(context.Background(), DepositFee, memo)
				}
			}
		}

		log.Info("Mint succesfull, saving cursor now")

		// save cursor
		cursor := tx.PagingToken()
		err = persistency.saveStellarCursor(cursor)
		if err != nil {
			log.Error("error while saving cursor:", err.Error())
			return
		}

	}

	// get saved cursor
	blockHeight, err := persistency.GetHeight()
	for err != nil {
		log.Warn("Error getting the bridge persistency", "error", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
			blockHeight, err = persistency.GetHeight()
		}
	}

	return w.StreamBridgeStellarTransactions(ctx, blockHeight.StellarCursor, transactionHandler)
}

// GetAccountDetails gets account details based an a Stellar address
func (w *stellarWallet) GetAccountDetails(address string) (account hProtocol.Account, err error) {
	client, err := w.GetHorizonClient()
	if err != nil {
		return hProtocol.Account{}, err
	}
	ar := horizonclient.AccountRequest{AccountID: address}
	account, err = client.AccountDetail(ar)
	if err != nil {
		return hProtocol.Account{}, errors.Wrapf(err, "failed to get account details for account: %s", address)
	}
	return account, nil
}

func (w *stellarWallet) StreamBridgeStellarTransactions(ctx context.Context, cursor string, handler func(op hProtocol.Transaction)) error {
	client, err := w.GetHorizonClient()
	if err != nil {
		return err
	}

	opRequest := horizonclient.TransactionRequest{
		ForAccount: w.keypair.Address(),
		Cursor:     cursor,
	}
	log.Info("Start fetching stellar transactions", "horizon", client.HorizonURL, "account", opRequest.ForAccount, "cursor", opRequest.Cursor)

	for {
		if ctx.Err() != nil {
			return nil
		}

		response, err := client.Transactions(opRequest)
		if err != nil {
			log.Info("Error getting transactions for stellar account", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
				continue
			}

		}
		for _, tx := range response.Embedded.Records {
			handler(tx)
			opRequest.Cursor = tx.PagingToken()
		}
		if len(response.Embedded.Records) == 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(10 * time.Second):
			}
		}

	}

}

func (w *stellarWallet) ScanBridgeAccount() error {
	return w.stellarTransactionStorage.ScanBridgeAccount()
}

func (w *stellarWallet) getTransactionEffects(txHash string) (effects horizoneffects.EffectsPage, err error) {
	client, err := w.GetHorizonClient()
	if err != nil {
		return effects, err
	}

	effectsReq := horizonclient.EffectRequest{
		ForTransaction: txHash,
	}
	effects, err = client.Effects(effectsReq)
	if err != nil {
		return effects, err
	}

	return effects, nil
}

func (w *stellarWallet) getOperationEffect(txHash string) (ops operations.OperationsPage, err error) {
	client, err := w.GetHorizonClient()
	if err != nil {
		return ops, err
	}

	opsRequest := horizonclient.OperationRequest{
		ForTransaction: txHash,
	}
	ops, err = client.Operations(opsRequest)
	if err != nil {
		return ops, err
	}

	return ops, nil
}

// GetHorizonClient gets the horizon client based on the wallet's network
func (w *stellarWallet) GetHorizonClient() (*horizonclient.Client, error) {
	switch w.config.StellarNetwork {
	case "testnet":
		return horizonclient.DefaultTestNetClient, nil
	case "production":
		return horizonclient.DefaultPublicNetClient, nil
	default:
		return nil, errors.New("network is not supported")
	}
}

// GetNetworkPassPhrase gets the Stellar network passphrase based on the wallet's network
func (w *stellarWallet) GetNetworkPassPhrase() string {
	switch w.config.StellarNetwork {
	case "testnet":
		return network.TestNetworkPassphrase
	case "production":
		return network.PublicNetworkPassphrase
	default:
		return network.TestNetworkPassphrase
	}
}

func (w *stellarWallet) GetAssetCodeAndIssuer() []string {
	switch w.config.StellarNetwork {
	case "testnet":
		return strings.Split(TFTTest, ":")
	case "production":
		return strings.Split(TFTMainnet, ":")
	default:
		return strings.Split(TFTTest, ":")
	}
}
