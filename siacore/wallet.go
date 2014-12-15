package siacore

import (
	"errors"
	"fmt"
	"io/ioutil"
	"sync"

	"github.com/NebulousLabs/Andromeda/consensus"
	"github.com/NebulousLabs/Andromeda/encoding"
	"github.com/NebulousLabs/Andromeda/signatures"
)

// Wallet in an interface that helps to build and sign transactions.
// Transactions are kept in wallet memory until they are signed, and referenced
// using a string id.
type WalletInterface interface {
	// Scan takes a state and looks for all of the outputs that it knows how to
	// spend.
	Scan(s *consensus.State) error

	// NewTransaction creates a fresh, empty transaction and returns an id that
	// can be used to reference the transaction.
	NewTransaction() (id string, err error)

	// RegisterTransaction creates a transaction out of an existing transaction
	// which can be modified by the wallet, returning an id that can be used to
	// reference the transaction.
	RegisterTransaction(*consensus.Transaction) (id string, err error)

	// FundTransaction will add `amount` to a transaction's inputs.
	FundTransaction(id string, amount consensus.Currency) error

	// AddFreeze will add `amount` of coins to a transaction that unlock at
	// block `release`.
	AddFreeze(id string, amount consensus.Currency, release consensus.BlockHeight) error

	// AddMinerFee adds a single miner fee of value `fee`.
	AddMinerFee(id string, fee consensus.Currency) error

	// AddOutput adds an output of value `amount` to address `ca`.
	AddOutput(id string, amount consensus.Currency, dest consensus.CoinAddress) error

	// AddFileContract adds a file contract to a transaction.
	AddFileContract(id string, fc consensus.FileContract) error

	// AddStorageProof adds a storage proof to a transaction.
	AddStorageProof(id string, sp consensus.StorageProof) error

	// AddArbitraryData adds a byte slice to the arbitrary data section of the
	// transaction.
	AddArbitraryData(id string, arb []byte) error

	// Sign transaction will sign the transaction associated with the id and
	// then return the transaction. Will not set the 'whole transaction' flag
	// when signing.
	SignPartialTransaction(id string) (consensus.Transaction, error)

	// Sign transaction will sign the transaction associated with the id and
	// then return the transaction. Will set the 'whole transaction' flag when
	// signing.
	SignWholeTransaction(id string) (consensus.Transaction, error)

	// SaveWallet creates a binary file containing keys and such so the coins
	// can be spent later.
	SaveWallet(filename string) error

	// LoadWallet is the inverse of SaveWallet, scooping up a wallet file and
	// now being able to use the addresses within.
	LoadWallet(filename string) error

	// A lock should be used whenever reads or writes are being done to the
	// wallet.
	sync.Locker
	// RLock()
	// RUnlock()
}

// Contains a secret key, the spend conditions associated with that key, the
// address associated with those spend conditions, and a list of outputs that
// the wallet knows how to spend.
type Wallet struct {
	state *consensus.State

	SecretKey       signatures.SecretKey
	SpendConditions consensus.SpendConditions

	OwnedOutputs map[consensus.OutputID]struct{} // A list of outputs spendable by this wallet.
	SpentOutputs map[consensus.OutputID]struct{} // A list of outputs spent by this wallet which may not yet be in the blockchain.
}

// Creates a new wallet that can receive and spend coins.
func CreateWallet(s *consensus.State) *Wallet {
	w := &Wallet{
		state:        s,
		OwnedOutputs: make(map[consensus.OutputID]struct{}),
		SpentOutputs: make(map[consensus.OutputID]struct{}),
	}

	sk, pk, err := signatures.GenerateKeyPair()
	if err != nil {
		panic(err)
	}
	w.SecretKey = sk
	w.SpendConditions.PublicKeys = append(w.SpendConditions.PublicKeys, pk)
	w.SpendConditions.NumSignatures = 1

	return w
}

// Scans all unspent transactions and adds the ones that are spendable by this
// wallet.
func (w *Wallet) Scan() {
	w.OwnedOutputs = make(map[consensus.OutputID]struct{})

	// Check for owned outputs from the standard SpendConditions.
	scanAddresses := make(map[consensus.CoinAddress]struct{})
	scanAddresses[w.SpendConditions.CoinAddress()] = struct{}{}

	// Get the matching set of outputs and add them to the OwnedOutputs map.
	w.state.RLock()
	for _, output := range w.state.ScanOutputs(scanAddresses) {
		w.OwnedOutputs[output] = struct{}{}
	}
	w.state.RUnlock()
}

// fundTransaction() adds `amount` Currency to the inputs, creating a refund
// output for any excess.
func (w *Wallet) FundTransaction(amount consensus.Currency, t *consensus.Transaction) (err error) {
	// Check that a nonzero amount of coins is being sent.
	if amount == consensus.Currency(0) {
		err = errors.New("cannot send 0 coins")
		return
	}

	// Add to the list of inputs until enough funds have been allocated.
	total := consensus.Currency(0)
	var newInputs []consensus.Input
	for id, _ := range w.OwnedOutputs {
		_, exists := w.SpentOutputs[id]
		if exists {
			continue
		}

		// Check that the output exists.
		var output consensus.Output
		output, err = w.state.Output(id)
		if err != nil {
			continue
		}

		// Create an input to add to the transaction.
		newInput := consensus.Input{
			OutputID:        id,
			SpendConditions: w.SpendConditions,
		}
		newInputs = append(newInputs, newInput)

		// Add the value of the output to the total and see if we've hit a
		// sufficient amount.
		total += output.Value
		if total >= amount {
			break
		}
	}

	// Check that the sum of the inputs is sufficient to complete the
	// transaction.
	if total < amount {
		err = fmt.Errorf("insufficient funds: %v, requested %v", total, amount)
		return
	}

	// Add all of the inputs to the transaction.
	t.Inputs = append(t.Inputs, newInputs...)

	// Add all of the inputs to the spent outputs map.
	for _, input := range newInputs {
		w.SpentOutputs[input.OutputID] = struct{}{}
	}

	// Add a refund output to the transaction if needed.
	if total-amount > 0 {
		t.Outputs = append(t.Outputs, consensus.Output{Value: total - amount, SpendHash: w.SpendConditions.CoinAddress()})
	}

	return
}

// Wallet.signTransaction() takes a transaction and adds a signature to the
// specified input.
func (w *Wallet) SignTransaction(t *consensus.Transaction, cf consensus.CoveredFields, inputIndex int) (err error) {
	input := t.Inputs[inputIndex]

	// Check that the spend conditions match.
	if input.SpendConditions.CoinAddress() != w.SpendConditions.CoinAddress() {
		err = errors.New("called SignTransaction on an unknown CoinAddress")
		return
	}

	// Create and append the signature struct.
	txnSig := consensus.TransactionSignature{
		InputID:       input.OutputID,
		CoveredFields: cf,
	}
	t.Signatures = append(t.Signatures, txnSig)

	// Hash the transaction according to the covered fields and produce
	// the cryptographic signature.
	sigHash := t.SigHash(len(t.Signatures) - 1)
	t.Signatures[len(t.Signatures)-1].Signature, err = signatures.SignBytes(sigHash[:], w.SecretKey)
	if err != nil {
		return
	}

	return
}

// Wallet.SpendCoins creates a transaction sending 'amount' to 'dest', and
// allocateding 'minerFee' as a miner fee. The transaction is submitted to the
// miner pool, but is also returned.
func (e *Environment) SpendCoins(amount, minerFee consensus.Currency, dest consensus.CoinAddress) (t consensus.Transaction, err error) {
	// Scan blockchain for outputs.
	e.wallet.Scan()

	// Add `amount` + `minerFee` coins to the transaction.
	err = e.wallet.FundTransaction(amount+minerFee, &t)
	if err != nil {
		return
	}

	// Add the miner fee.
	t.MinerFees = append(t.MinerFees, minerFee)

	// Add the output to `dest`.
	t.Outputs = append(t.Outputs, consensus.Output{Value: amount, SpendHash: dest})

	// Sign each input.
	for i := range t.Inputs {
		err = e.wallet.SignTransaction(&t, consensus.CoveredFields{WholeTransaction: true}, i)
		if err != nil {
			return
		}
	}

	// Send the transaction to the environment.
	e.AcceptTransaction(t)

	return
}

// WalletBalance counts up the total number of coins that the wallet knows how
// to spend, according to the State. WalletBalance will ignore all unconfirmed
// transactions that have been created.
func (e *Environment) WalletBalance() consensus.Currency {
	e.wallet.Scan()

	total := consensus.Currency(0)
	for id, _ := range e.wallet.OwnedOutputs {
		// Check that the output exists.
		var output consensus.Output
		output, err := e.state.Output(id)
		if err != nil {
			continue
		}

		total += output.Value
	}

	return total
}

// Environment.CoinAddress returns the CoinAddress which foreign coins should
// be sent to.
func (e *Environment) CoinAddress() consensus.CoinAddress {
	return e.wallet.SpendConditions.CoinAddress()
}

// SaveCoinAddress saves the address of the wallet used within the environment.
func (e *Environment) SaveCoinAddress(filename string) (err error) {
	pubKeyBytes := encoding.Marshal(e.wallet.SpendConditions.CoinAddress())

	// Open the file and write the key to the filename.
	err = ioutil.WriteFile(filename, pubKeyBytes, 0666)
	if err != nil {
		return
	}

	return
}

func (e *Environment) SaveSecretKey(filename string) (err error) {
	return
}

func (e *Environment) LoadSecretKey(filename string) (err error) {
	return
}
