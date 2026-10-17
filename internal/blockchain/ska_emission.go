// Copyright (c) 2025 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/chaincfg/chainhash"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/dcrec/secp256k1"
	"github.com/monetarium/monetarium-node/dcrec/secp256k1/ecdsa"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/txscript/stdaddr"
	"github.com/monetarium/monetarium-node/wire"
)

// ChainStateProvider defines the interface for accessing blockchain state
// needed for SKA emission validation.
type ChainStateProvider interface {
	HasSKAEmissionOccurred(cointype.CoinType) bool
	GetSKAEmissionNonce(cointype.CoinType) uint64
}

// isSKAEmissionWindow returns whether the provided block height is within
// the emission window for the specified SKA coin type.
// Note: This function only checks the emission window bounds, not stake validation.
// Stake validation is checked separately in ValidateAuthorizedSKAEmissionTransaction.
func isSKAEmissionWindow(blockHeight int64, coinType cointype.CoinType, chainParams *chaincfg.Params) bool {
	config, exists := chainParams.SKACoins[coinType]
	if !exists {
		return false
	}

	emissionStart := int64(config.EmissionHeight)
	emissionEnd := emissionStart + int64(config.EmissionWindow)

	return blockHeight >= emissionStart && blockHeight <= emissionEnd
}

// isSKAEmissionWindowActive returns whether any SKA coin type has an active
// emission window at the specified block height.
func isSKAEmissionWindowActive(blockHeight int64, chainParams *chaincfg.Params) bool {
	for coinType := range chainParams.SKACoins {
		if isSKAEmissionWindow(blockHeight, coinType, chainParams) {
			return true
		}
	}
	return false
}

// hasVotePassed checks if a consensus vote has passed and is active at the
// given block node. This is used to validate SKA2+ activations require
// stakeholder approval through the voting system.
func (b *BlockChain) hasVotePassed(voteID string, prevNode *blockNode) bool {
	deployment, ok := b.deploymentData[voteID]
	if !ok {
		return false
	}

	state := b.deploymentState(prevNode, &deployment)
	return state.State == ThresholdActive
}

// HasVotePassedAtHeight checks if a consensus vote has passed and is active at
// the given block height. This is a convenience wrapper around hasVotePassed
// that looks up the block node by height.
func (b *BlockChain) HasVotePassedAtHeight(voteID string, blockHeight int64) bool {
	// We need to check the vote state at the parent of the block being validated
	// because deployment states are calculated based on the parent block
	b.chainLock.RLock()
	defer b.chainLock.RUnlock()

	// Find the node at blockHeight - 1 (the parent of the block being validated)
	node := b.bestChain.NodeByHeight(blockHeight - 1)
	if node == nil {
		// If we can't find the node, vote hasn't passed
		return false
	}

	return b.hasVotePassed(voteID, node)
}

// skaEmissionStateError describes an SKA emission that conflicts with the
// recorded emission state, such as an unexpected nonce or an emitted coin type.
type skaEmissionStateError string

// Error satisfies the error interface.
func (e skaEmissionStateError) Error() string {
	return string(e)
}

// isSKAEmissionStateError returns whether err is or wraps a
// skaEmissionStateError.
func isSKAEmissionStateError(err error) bool {
	var stateErr skaEmissionStateError
	return errors.As(err, &stateErr)
}

// CheckSKAEmissionAlreadyExists checks if a coin type has already been emitted
// in the blockchain. This now uses the persistent blockchain state for O(1)
// lookups instead of scanning blocks.
func CheckSKAEmissionAlreadyExists(coinType cointype.CoinType, chain ChainStateProvider) bool {
	// Use blockchain state for efficient and reliable emission tracking
	return chain.HasSKAEmissionOccurred(coinType)
}

// CreateAuthorizedSKAEmissionTransaction creates a cryptographically authorized
// SKA emission transaction with proper security controls including signature
// verification and replay protection.
//
// It's  a reference implementation for the emission transaction creation, the actual
// transaction creation is handled by the monwallet's createUnsignedSKAEmissionTransaction.
//
// NOTE: This function validates the authorization but does NOT verify the signature.
// The signature will be verified later during transaction validation. This is because
// the signature must bind to the final transaction hash, which is only available
// after the transaction is fully constructed.
func CreateAuthorizedSKAEmissionTransaction(auth *chaincfg.SKAEmissionAuth,
	emissionAddresses []string, amounts []*big.Int,
	chainParams *chaincfg.Params) (*wire.MsgTx, error) {

	// Validate authorization structure
	if auth == nil {
		return nil, fmt.Errorf("SKA emission authorization required")
	}

	if auth.EmissionKey == nil {
		return nil, fmt.Errorf("SKA emission key required")
	}

	if len(auth.Signature) == 0 {
		return nil, fmt.Errorf("SKA emission signature required")
	}

	// Validate coin type
	if auth.CoinType < 1 || auth.CoinType > 255 {
		return nil, fmt.Errorf("invalid SKA coin type: %d", auth.CoinType)
	}

	// Check if emission is authorized for this coin type
	authorizedKey := chainParams.GetSKAEmissionKey(auth.CoinType)
	if authorizedKey == nil {
		return nil, fmt.Errorf("no emission key configured for coin type %d", auth.CoinType)
	}

	// Verify the provided key matches the authorized key
	if !bytes.Equal(auth.EmissionKey.SerializeCompressed(),
		authorizedKey.SerializeCompressed()) {
		return nil, fmt.Errorf("unauthorized emission key for coin type %d", auth.CoinType)
	}

	// NOTE: Nonce checking is NOT performed during transaction creation
	// because wallets cannot reliably know the chain state due to reorgs and lag.
	// The nonce will be validated during block acceptance in validateEmissionAuthorization
	// which uses the actual blockchain state for proper replay protection.

	// Validate emission amounts
	if len(emissionAddresses) != len(amounts) {
		return nil, fmt.Errorf("emission addresses and amounts length mismatch")
	}

	if len(emissionAddresses) == 0 {
		return nil, fmt.Errorf("no emission addresses specified")
	}

	zero := new(big.Int)
	totalAmount := new(big.Int)
	for _, amount := range amounts {
		if amount == nil || amount.Cmp(zero) <= 0 {
			return nil, fmt.Errorf("invalid emission amount: %s", amount)
		}
		totalAmount.Add(totalAmount, amount)
	}

	// Verify total matches authorization
	if totalAmount.Cmp(auth.Amount) != 0 {
		return nil, fmt.Errorf("total emission amount %s does not match authorization %s",
			totalAmount.String(), auth.Amount.String())
	}

	// Get the SKA coin config for this coin type
	skaConfig, exists := chainParams.SKACoins[auth.CoinType]
	if !exists {
		return nil, fmt.Errorf("SKA coin type %d not configured", auth.CoinType)
	}

	// Verify emission height is within the emission window
	emissionStart := int64(skaConfig.EmissionHeight)
	emissionEnd := emissionStart + int64(skaConfig.EmissionWindow)
	if auth.Height < emissionStart || auth.Height > emissionEnd {
		return nil, fmt.Errorf("emission height %d is outside emission window [%d, %d] for coin type %d",
			auth.Height, emissionStart, emissionEnd, auth.CoinType)
	}

	// NOTE: We do NOT verify the signature here because it must bind to the
	// transaction hash, which we haven't computed yet. The signature will be
	// verified during transaction validation in ValidateAuthorizedSKAEmissionTransaction.

	// Create the authorized emission transaction with Expiry set to window end
	// This ensures automatic mempool cleanup if the emission window expires
	tx := &wire.MsgTx{
		SerType:  wire.TxSerializeFull,
		Version:  1,
		LockTime: 0,
		Expiry:   uint32(emissionEnd),
	}

	// Create signature script with authorization data
	authScript, err := createEmissionAuthScript(auth)
	if err != nil {
		return nil, fmt.Errorf("failed to create authorization script: %w", err)
	}

	// Add authorized input
	tx.TxIn = append(tx.TxIn, &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{}, // All zeros
			Index: 0xffffffff,       // Max value indicates null
			Tree:  wire.TxTreeRegular,
		},
		SignatureScript: authScript,
		Sequence:        0xffffffff,
		BlockHeight:     wire.NullBlockHeight,
		BlockIndex:      wire.NullBlockIndex,
		ValueIn:         wire.NullValueIn,
	})

	// Add outputs for each emission address
	for i, addressStr := range emissionAddresses {
		addr, err := stdaddr.DecodeAddress(addressStr, chainParams)
		if err != nil {
			return nil, fmt.Errorf("invalid emission address %s: %w", addressStr, err)
		}

		// Create script for the address
		_, pkScript := addr.PaymentScript()

		// Add SKA output with specific coin type
		// Force script version 0 to match validation requirements
		// Validation only accepts version 0 (line 554) for consistency
		// For SKA coins, we use SKAValue (big.Int) for the amount
		tx.TxOut = append(tx.TxOut, &wire.TxOut{
			Value:    0,
			SKAValue: amounts[i],
			CoinType: auth.CoinType,
			Version:  0,
			PkScript: pkScript,
		})
	}

	return tx, nil
}

// createEmissionAuthScript creates the signature script containing authorization
// data for the emission transaction. This embeds the authorization proof in
// the transaction itself for validation.
//
// Script format v3 (for big.Int amounts):
// [SKA_marker:4][auth_version:1][nonce:8][coin_type:1][amount_len:1][amount:N][height:8][pubkey:33][sig_len:1][signature:var]
func createEmissionAuthScript(auth *chaincfg.SKAEmissionAuth) ([]byte, error) {
	var script bytes.Buffer

	// Standard SKA emission marker
	script.Write([]byte{0x01, 0x53, 0x4b, 0x41}) // "SKA" marker

	// Authorization data - version 0x03 for big.Int amounts
	script.WriteByte(0x03) // Auth version 3

	// Nonce (8 bytes)
	nonceBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(nonceBytes, auth.Nonce)
	script.Write(nonceBytes)

	// Coin type (1 byte)
	script.WriteByte(uint8(auth.CoinType))

	// Amount (variable length big.Int)
	// Write length prefix (1 byte) followed by big-endian bytes
	amountBytes := auth.Amount.Bytes() // Big-endian representation
	if len(amountBytes) > 255 {
		return nil, fmt.Errorf("amount too large: %d bytes (max 255)", len(amountBytes))
	}
	script.WriteByte(uint8(len(amountBytes)))
	script.Write(amountBytes)

	// Height (8 bytes)
	heightBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(heightBytes, uint64(auth.Height))
	script.Write(heightBytes)

	// Public key (33 bytes compressed)
	pubKeyBytes := auth.EmissionKey.SerializeCompressed()
	script.Write(pubKeyBytes)

	// Signature length and signature
	script.WriteByte(uint8(len(auth.Signature)))
	script.Write(auth.Signature)

	return script.Bytes(), nil
}

// ValidateAuthorizedSKAEmissionTransaction validates that a transaction is a valid
// cryptographically authorized SKA emission transaction with full security controls
// including signature verification and replay protection.
//
// This performs comprehensive validation beyond the fast detection in
// wire.IsSKAEmissionTransaction, including:
// - Cryptographic signature verification
// - Nonce-based replay protection
// - Emission window validation
// - Authorization amount matching
// - Governance parameter enforcement
//
// Note: Stakeholder vote activation (for SKA2+) is checked at block validation level
// in CheckSKAEmissionInBlock, not here, to allow mempool to accept transactions before
// vote passes.
func ValidateAuthorizedSKAEmissionTransaction(tx *wire.MsgTx, blockHeight int64,
	chain ChainStateProvider, chainParams *chaincfg.Params) error {

	// Check if this is within a valid emission window for any SKA coin type
	// We need to check the transaction outputs to determine the coin type
	if len(tx.TxOut) == 0 {
		return fmt.Errorf("SKA emission transaction must have at least 1 output")
	}

	// Get the coin type from the first output
	coinType := tx.TxOut[0].CoinType

	// Ensure SKA emissions only happen after stake validation is active
	// This ensures the SSFee mechanism is available for distributing non-VAR fees
	stakeValidationHeight := chainParams.StakeValidationHeight
	if blockHeight < stakeValidationHeight {
		return fmt.Errorf("SKA emission not allowed before stake validation height %d (attempted at %d)",
			stakeValidationHeight, blockHeight)
	}

	if !isSKAEmissionWindow(blockHeight, coinType, chainParams) {
		return fmt.Errorf("SKA emission transaction at invalid height %d for coin type %d",
			blockHeight, coinType)
	}

	// Note: Stakeholder vote check for SKA2+ is performed in CheckSKAEmissionInBlock
	// at the block validation level, not here at transaction validation level.
	// This allows the mempool to accept emission transactions before the vote passes.

	// Validate transaction structure
	if len(tx.TxIn) != 1 {
		return fmt.Errorf("SKA emission transaction must have exactly 1 input, got %d",
			len(tx.TxIn))
	}

	// Validate null input (similar to coinbase validation)
	prevOut := tx.TxIn[0].PreviousOutPoint
	if !prevOut.Hash.IsEqual(&chainhash.Hash{}) || prevOut.Index != 0xffffffff {
		return fmt.Errorf("SKA emission transaction input is not null")
	}

	// Extract and validate authorization from signature script
	auth, err := extractEmissionAuthorization(tx.TxIn[0].SignatureScript)
	if err != nil {
		return fmt.Errorf("invalid emission authorization: %w", err)
	}

	// Validate authorization against chain parameters.  A conflict with the
	// recorded emission state is only reported once every other check has
	// passed.
	stateErr := validateEmissionAuthorization(auth, chain, chainParams)
	if stateErr != nil {
		stateErr = fmt.Errorf("emission authorization validation failed: %w",
			stateErr)
		if !isSKAEmissionStateError(stateErr) {
			return stateErr
		}
	}

	// CRITICAL Verify the cryptographic signature
	// This binds the signature to the exact transaction being validated
	if err := verifyEmissionSignature(tx, auth, blockHeight, chainParams); err != nil {
		return fmt.Errorf("emission signature verification failed: %w", err)
	}

	// Determine expected coin type from first output
	var emissionCoinType cointype.CoinType
	totalEmissionAmount := new(big.Int)

	// Validate all outputs have consistent coin type and valid amounts
	for i, txOut := range tx.TxOut {
		if i == 0 {
			// Set expected coin type from first output
			emissionCoinType = txOut.CoinType

			// Ensure coin type is SKA using the standard check
			if !emissionCoinType.IsSKA() {
				return fmt.Errorf("emission coin type %d is not an SKA coin type", emissionCoinType)
			}
		} else {
			// Verify all outputs use the same coin type
			if txOut.CoinType != emissionCoinType {
				return fmt.Errorf("inconsistent coin types: output 0 has %d, output %d has %d",
					emissionCoinType, i, txOut.CoinType)
			}
		}

		// SKA emissions use SKAValue (big.Int) for amounts
		if txOut.SKAValue == nil || txOut.SKAValue.Sign() <= 0 {
			return fmt.Errorf("SKA emission transaction output %d has invalid amount (nil or non-positive)",
				i)
		}

		// For SKA coins, get max supply from per-coin config (big.Int)
		skaConfig := chainParams.GetSKACoinConfig(txOut.CoinType)
		if skaConfig != nil && skaConfig.MaxSupply != nil {
			// Compare using big.Int
			if txOut.SKAValue.Cmp(skaConfig.MaxSupply) > 0 {
				return fmt.Errorf("SKA emission transaction output %d exceeds maximum %s for coin type %d",
					i, skaConfig.MaxSupply.String(), txOut.CoinType)
			}
		}
		// Note: If config or MaxSupply is nil, we skip this check and rely on
		// the later total emission amount check against configured amounts

		// Validate script version to prevent unspendable emissions
		// Only allow known script versions (currently 0 for P2PKH/P2SH)
		if txOut.Version != 0 {
			return fmt.Errorf("SKA emission output %d has unsupported script version %d (only version 0 allowed)",
				i, txOut.Version)
		}

		totalEmissionAmount.Add(totalEmissionAmount, txOut.SKAValue)
	}

	// Verify authorization coin type matches transaction outputs
	if auth.CoinType != emissionCoinType {
		return fmt.Errorf("authorization coin type %d does not match transaction outputs %d",
			auth.CoinType, emissionCoinType)
	}

	// Verify authorization amount matches transaction total
	if auth.Amount.Cmp(totalEmissionAmount) != 0 {
		return fmt.Errorf("authorization amount %s does not match transaction total %s",
			auth.Amount.String(), totalEmissionAmount.String())
	}

	// Enforce governance-configured emission limits
	// Prevent authorized keys from creating emissions exceeding governance parameters
	skaConfig, exists := chainParams.SKACoins[emissionCoinType]
	if !exists {
		return fmt.Errorf("SKA coin type %d not configured in chain params", emissionCoinType)
	}

	// Bind every output to the governance-configured emission distribution: the
	// transaction must have exactly the configured number of outputs, paying the
	// configured addresses, in the configured order, with the configured amounts.
	// Without this, the emission key holder could redirect the entire pre-emitted
	// supply to arbitrary addresses while still satisfying signature and
	// total-amount checks.
	if len(skaConfig.EmissionAddresses) != len(skaConfig.EmissionAmounts) {
		return fmt.Errorf("SKA coin type %d has misconfigured emission distribution: "+
			"%d addresses vs %d amounts", emissionCoinType,
			len(skaConfig.EmissionAddresses), len(skaConfig.EmissionAmounts))
	}
	if len(tx.TxOut) != len(skaConfig.EmissionAddresses) {
		return fmt.Errorf("SKA coin type %d emission transaction has %d outputs, "+
			"governance config requires %d", emissionCoinType,
			len(tx.TxOut), len(skaConfig.EmissionAddresses))
	}

	expectedEmissionAmount := new(big.Int)
	for i, txOut := range tx.TxOut {
		expectedAddrStr := skaConfig.EmissionAddresses[i]
		expectedAmount := skaConfig.EmissionAmounts[i]
		if expectedAmount == nil {
			return fmt.Errorf("SKA coin type %d emission amount %d is not configured",
				emissionCoinType, i)
		}

		expectedAddr, err := stdaddr.DecodeAddress(expectedAddrStr, chainParams)
		if err != nil {
			return fmt.Errorf("SKA coin type %d has invalid emission address %d (%q): %w",
				emissionCoinType, i, expectedAddrStr, err)
		}
		expectedVersion, expectedScript := expectedAddr.PaymentScript()
		if expectedVersion != 0 {
			return fmt.Errorf("SKA coin type %d emission address %d (%s) requires "+
				"unsupported script version %d", emissionCoinType, i,
				expectedAddrStr, expectedVersion)
		}

		if !bytes.Equal(txOut.PkScript, expectedScript) {
			return fmt.Errorf("SKA emission output %d does not pay the configured "+
				"emission address %s for coin type %d", i, expectedAddrStr, emissionCoinType)
		}
		if txOut.SKAValue.Cmp(expectedAmount) != 0 {
			return fmt.Errorf("SKA emission output %d amount %s does not match "+
				"governance-configured amount %s for address %s (coin type %d)",
				i, txOut.SKAValue.String(), expectedAmount.String(),
				expectedAddrStr, emissionCoinType)
		}

		expectedEmissionAmount.Add(expectedEmissionAmount, expectedAmount)
	}

	// Defense-in-depth: the per-output amount check above already implies the
	// totals match, but keep this as a guarded invariant in case a future refactor
	// changes one path without the other.
	if expectedEmissionAmount.Sign() > 0 && totalEmissionAmount.Cmp(expectedEmissionAmount) != 0 {
		return fmt.Errorf("total emission %s does not match governance-configured amount %s for coin type %d",
			totalEmissionAmount.String(), expectedEmissionAmount.String(), emissionCoinType)
	}

	// Validate auth.Height is within the emission window
	// This allows mempool broadcasting without per-block re-signing
	emissionStart := int64(skaConfig.EmissionHeight)
	emissionEnd := emissionStart + int64(skaConfig.EmissionWindow)
	if auth.Height < emissionStart || auth.Height > emissionEnd {
		return fmt.Errorf("authorization height %d is outside emission window [%d, %d] for coin type %d",
			auth.Height, emissionStart, emissionEnd, emissionCoinType)
	}

	// The current block must also be within the emission window
	if blockHeight < emissionStart || blockHeight > emissionEnd {
		return fmt.Errorf("current block height %d is outside emission window [%d, %d] for coin type %d",
			blockHeight, emissionStart, emissionEnd, emissionCoinType)
	}

	// Validate transaction parameters
	if tx.LockTime != 0 {
		return fmt.Errorf("SKA emission transaction must have LockTime 0")
	}

	// Expiry must be set to the emission window end for automatic mempool cleanup
	// This prevents the transaction from lingering in mempool if the window expires
	if tx.Expiry == 0 || int64(tx.Expiry) > emissionEnd {
		return fmt.Errorf("SKA emission Expiry must be set to emission window end (block %d), got %d",
			emissionEnd, tx.Expiry)
	}

	// Note: Nonce is NOT updated here during validation to avoid side effects.
	// The nonce will be updated when the block is successfully connected to the chain
	// in CheckSKAEmissionInBlock to ensure replay protection only after commitment.

	return stateErr
}

// verifyEmissionSignature verifies the cryptographic signature of an emission transaction.
// This is a CRITICAL security function that prevents:
// - Miner redirect attacks (changing outputs)
// - Signature tampering
// - Cross-network replay attacks
//
// The signature binds to:
// - The exact transaction outputs (via no-witness serialization hash)
// - The network ID (preventing cross-network replay)
// - The coin type, nonce, and authorization height (for window-based validation)
func verifyEmissionSignature(tx *wire.MsgTx, auth *chaincfg.SKAEmissionAuth,
	_ int64, chainParams *chaincfg.Params) error {

	// Compute the transaction hash using explicit no-witness serialization
	// This ensures the signature binds to the exact outputs without witness data
	// BytesPrefix() is explicitly documented to use TxSerializeNoWitness
	txBytes, err := tx.BytesPrefix() // Uses wire.TxSerializeNoWitness internally
	if err != nil {
		return fmt.Errorf("failed to serialize transaction (no-witness): %w", err)
	}
	txHash := sha256.Sum256(txBytes)

	// Build the domain-separated signing message
	// Format: "SKA-EMIT-V2" || netID || coinType || nonce || authHeight || txHash
	var msgBuf bytes.Buffer

	// Domain separator to prevent signature reuse in other contexts
	msgBuf.WriteString("SKA-EMIT-V2")

	// Network ID for replay protection across networks
	if err := binary.Write(&msgBuf, binary.LittleEndian, uint32(chainParams.Net)); err != nil {
		return fmt.Errorf("failed to write network ID: %w", err)
	}

	// Coin type
	msgBuf.WriteByte(byte(auth.CoinType))

	// Nonce for replay protection within network
	if err := binary.Write(&msgBuf, binary.LittleEndian, auth.Nonce); err != nil {
		return fmt.Errorf("failed to write nonce: %w", err)
	}

	// Use auth.Height (signed by emitter) instead of current blockHeight
	// This allows broadcasting to mempool and inclusion at any valid height within window
	if err := binary.Write(&msgBuf, binary.LittleEndian, uint64(auth.Height)); err != nil {
		return fmt.Errorf("failed to write authorization height: %w", err)
	}

	// Transaction hash - this binds the signature to exact outputs
	msgBuf.Write(txHash[:])

	// Create the final message hash
	msgHash := sha256.Sum256(msgBuf.Bytes())

	// Parse the signature with strict DER validation
	sig, err := ecdsa.ParseDERSignature(auth.Signature)
	if err != nil {
		return fmt.Errorf("invalid DER signature format: %w", err)
	}

	// Enforce canonical signature encoding (low-S) to prevent malleability
	// In ECDSA, both S and -S (mod n) are valid signatures, but we enforce low-S
	// where S <= n/2 to ensure a canonical form
	sigS := sig.S()
	if sigS.IsOverHalfOrder() {
		return fmt.Errorf("signature not canonical: S value is not low (S > n/2)")
	}

	// Additional strict DER checks for consensus safety
	if len(auth.Signature) > 73 {
		return fmt.Errorf("signature too long: %d bytes (max 73)", len(auth.Signature))
	}

	// Verify the signature against the message and public key
	if !sig.Verify(msgHash[:], auth.EmissionKey) {
		return fmt.Errorf("signature verification failed - unauthorized emission attempt")
	}

	// Signature verified successfully

	return nil
}

// extractEmissionAuthorization extracts the emission authorization from a signature script.
// Supports both v2 (int64 amount) and v3 (big.Int amount) formats:
// v2: [SKA_marker:4][auth_version:1][nonce:8][coin_type:1][amount:8][height:8][pubkey:33][sig_len:1][signature:var]
// v3: [SKA_marker:4][auth_version:1][nonce:8][coin_type:1][amount_len:1][amount:N][height:8][pubkey:33][sig_len:1][signature:var]
func extractEmissionAuthorization(sigScript []byte) (*chaincfg.SKAEmissionAuth, error) {

	// Calculate minimum required length for v3: 4(marker) + 1(version) + 8(nonce) + 1(cointype) + 1(amtlen) + 0(amt) + 8(height) + 33(pubkey) + 1(siglen)
	const minScriptLenV3 = 4 + 1 + 8 + 1 + 1 + 0 + 8 + 33 + 1 // = 57 bytes minimum

	if len(sigScript) < minScriptLenV3 {
		return nil, fmt.Errorf("signature script too short: %d bytes, need at least %d bytes", len(sigScript), minScriptLenV3)
	}

	// Check SKA marker
	if len(sigScript) < 4 || !bytes.Equal(sigScript[0:4], []byte{0x01, 0x53, 0x4b, 0x41}) {
		return nil, fmt.Errorf("missing SKA emission marker")
	}

	offset := 4

	// Check authorization version
	authVersion := sigScript[offset]
	if authVersion != 0x03 {
		return nil, fmt.Errorf("unsupported authorization version: %d (supported: 3)", authVersion)
	}
	offset++

	// Extract nonce (8 bytes)
	if len(sigScript) < offset+8 {
		return nil, fmt.Errorf("insufficient data for nonce")
	}
	nonce := binary.LittleEndian.Uint64(sigScript[offset : offset+8])
	offset += 8

	// Extract coin type (1 byte)
	if len(sigScript) < offset+1 {
		return nil, fmt.Errorf("insufficient data for coin type")
	}
	coinType := cointype.CoinType(sigScript[offset])
	offset++

	// Extract amount (variable-length big.Int, length-prefixed, big-endian)
	if len(sigScript) < offset+1 {
		return nil, fmt.Errorf("insufficient data for amount length")
	}
	amountLen := int(sigScript[offset])
	offset++

	if len(sigScript) < offset+amountLen {
		return nil, fmt.Errorf("insufficient data for amount: need %d bytes, have %d", amountLen, len(sigScript)-offset)
	}
	amount := new(big.Int).SetBytes(sigScript[offset : offset+amountLen])
	offset += amountLen

	// Extract height (8 bytes)
	if len(sigScript) < offset+8 {
		return nil, fmt.Errorf("insufficient data for height at offset %d, have %d bytes, need %d", offset, len(sigScript), offset+8)
	}
	height := int64(binary.LittleEndian.Uint64(sigScript[offset : offset+8]))
	offset += 8

	// Extract public key (33 bytes compressed)
	if len(sigScript) < offset+33 {
		return nil, fmt.Errorf("insufficient data for public key at offset %d, have %d bytes, need %d", offset, len(sigScript), offset+33)
	}
	pubKeyBytes := sigScript[offset : offset+33]

	// Validate public key format
	if len(pubKeyBytes) != 33 {
		return nil, fmt.Errorf("invalid public key length: expected 33 bytes, got %d", len(pubKeyBytes))
	}
	if pubKeyBytes[0] != 0x02 && pubKeyBytes[0] != 0x03 {
		return nil, fmt.Errorf("invalid public key format: first byte should be 0x02 or 0x03 for compressed keys, got 0x%02x", pubKeyBytes[0])
	}

	pubKey, err := secp256k1.ParsePubKey(pubKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid public key: %w", err)
	}
	offset += 33

	// Extract signature length
	if len(sigScript) < offset+1 {
		return nil, fmt.Errorf("insufficient data for signature length")
	}
	sigLen := int(sigScript[offset])
	offset++

	// Extract signature
	if len(sigScript) < offset+sigLen {
		return nil, fmt.Errorf("insufficient data for signature: need %d, have %d",
			sigLen, len(sigScript)-offset)
	}
	signature := sigScript[offset : offset+sigLen]

	return &chaincfg.SKAEmissionAuth{
		EmissionKey: pubKey,
		Signature:   signature,
		Nonce:       nonce,
		CoinType:    coinType,
		Amount:      amount,
		Height:      height,
	}, nil
}

// validateEmissionAuthorization validates the cryptographic authorization
// against the chain parameters and verifies the signature.
func validateEmissionAuthorization(auth *chaincfg.SKAEmissionAuth, chain ChainStateProvider, chainParams *chaincfg.Params) error {
	// Check if emission is authorized for this coin type
	authorizedKey := chainParams.GetSKAEmissionKey(auth.CoinType)
	if authorizedKey == nil {
		return fmt.Errorf("no emission key configured for coin type %d", auth.CoinType)
	}

	// Verify the provided key matches the authorized key
	if !bytes.Equal(auth.EmissionKey.SerializeCompressed(),
		authorizedKey.SerializeCompressed()) {
		return fmt.Errorf("unauthorized emission key for coin type %d", auth.CoinType)
	}

	// Check nonce for replay protection - must be exactly one more than the current nonce
	// Use blockchain state instead of chainParams for proper persistence
	currentNonce := chain.GetSKAEmissionNonce(auth.CoinType)
	expectedNonce := currentNonce + 1
	if auth.Nonce != expectedNonce {
		return skaEmissionStateError(fmt.Sprintf("invalid nonce: expected %d, "+
			"got %d", expectedNonce, auth.Nonce))
	}

	return nil
}

// CheckSKAEmissionInBlock validates SKA emission rules for a block at the given height.
// This function enforces:
// 1. SKA emission windows allow emission transactions during defined periods
// 2. Non-emission windows must not contain any SKA emission transactions
// 3. No SKA transactions are allowed before activation height
// 4. Each coin type can only be emitted once (first valid emission wins)
//
// This function is called with the chain lock held and must not acquire it again.
func CheckSKAEmissionInBlock(block *dcrutil.Block, prevNode *blockNode,
	chain *BlockChain, chainParams *chaincfg.Params) error {

	// Calculate block height from prevNode
	blockHeight := int64(1)
	if prevNode != nil {
		blockHeight = prevNode.height + 1
	}

	// Check if any emission window is active
	isEmissionWindowActive := isSKAEmissionWindowActive(blockHeight, chainParams)

	// Check if any SKA coins with transactions in this block are active

	var emissionTxCount int
	var skaTxCount int
	emissionTxCoinTypes := make(map[cointype.CoinType]bool)

	// A conflict with the recorded emission state is only reported once every
	// other check has passed.
	var stateErr error

	// Check all transactions in the block
	for i, tx := range block.Transactions() {
		msgTx := tx.MsgTx()

		// Count SKA emission transactions
		if wire.IsSKAEmissionTransaction(msgTx) {
			emissionTxCount++

			// Validate the emission transaction with full cryptographic authorization
			if err := ValidateAuthorizedSKAEmissionTransaction(msgTx, blockHeight, chain, chainParams); err != nil {
				err = fmt.Errorf("invalid SKA emission transaction at index %d: %w", i, err)
				if !isSKAEmissionStateError(err) {
					return err
				}
				if stateErr == nil {
					stateErr = err
				}
			}

			// Note: Nonce update is handled separately during actual block connection
			// to avoid double-updates during validation phases.

			// Track which coin types are being emitted and check for previous emissions
			for _, txOut := range msgTx.TxOut {
				coinType := txOut.CoinType

				// Check for multiple emission transactions in the same block
				if emissionTxCoinTypes[coinType] {
					return fmt.Errorf("multiple emission transactions for coin type %d at height %d - only one emission per coin type allowed", coinType, blockHeight)
				}

				// For SKA2 and higher coin types, verify stakeholder vote has passed
				// SKA1 is always active and doesn't require voting
				if coinType >= 2 {
					voteID := fmt.Sprintf("activateska%d", coinType)
					// Use hasVotePassed with prevNode to avoid re-acquiring the chain lock
					// that the caller already holds (prevents deadlock)
					if !chain.hasVotePassed(voteID, prevNode) {
						return fmt.Errorf("cannot emit %s at height %d: stakeholder vote %s has not activated this coin type",
							coinType, blockHeight, voteID)
					}
				}

				// Check if this coin type has already been emitted in previous blocks
				// This uses the blockchain state for O(1) lookups and proper reorg handling
				if CheckSKAEmissionAlreadyExists(coinType, chain) && stateErr == nil {
					stateErr = skaEmissionStateError(fmt.Sprintf("SKA coin type %d "+
						"has already been emitted - only one emission per coin "+
						"type allowed", coinType))
				}

				emissionTxCoinTypes[coinType] = true
			}
		}

		// Count transactions with SKA outputs (excluding emission transactions)
		if !wire.IsSKAEmissionTransaction(msgTx) {
			for _, txOut := range msgTx.TxOut {
				if txOut.CoinType.IsSKA() {
					skaTxCount++
					break
				}
			}
		}
	}

	// Validate emission rules based on block height
	if isEmissionWindowActive {
		// Emission transactions are allowed during emission windows
		if emissionTxCount > 0 {
			// Validate that emission transactions are within their respective windows
			for coinType := range emissionTxCoinTypes {
				if !isSKAEmissionWindow(blockHeight, coinType, chainParams) {
					config := chainParams.SKACoins[coinType]
					emissionStart := int64(config.EmissionHeight)
					emissionEnd := emissionStart + int64(config.EmissionWindow)
					return fmt.Errorf("emission transaction for coin type %d at height %d is outside emission window (%d-%d)",
						coinType, blockHeight, emissionStart, emissionEnd)
				}
				// Emission transaction validated successfully
			}
		}
	} else {
		// Non-emission windows must not have emission transactions
		if emissionTxCount > 0 {
			return fmt.Errorf("block at height %d contains %d SKA emission transactions but is not in emission window",
				blockHeight, emissionTxCount)
		}
	}

	// Check that all SKA transactions use active coin types
	// Collect all coin types used in non-emission SKA transactions
	usedCoinTypes := make(map[cointype.CoinType]bool)
	for _, tx := range block.Transactions() {
		msgTx := tx.MsgTx()
		if !wire.IsSKAEmissionTransaction(msgTx) {
			for _, txOut := range msgTx.TxOut {
				if txOut.CoinType.IsSKA() {
					usedCoinTypes[txOut.CoinType] = true
				}
			}
		}
	}

	// Verify all used coin types are active
	for coinType := range usedCoinTypes {
		if !chainParams.IsSKACoinTypeActive(coinType) {
			return fmt.Errorf("SKA transactions not allowed for inactive coin type %d", coinType)
		}
	}

	return stateErr
}

// extractSKAEmissionsFromBlock extracts all SKA emission records from a block.
// This is used during block connection/disconnection to update emission state.
func extractSKAEmissionsFromBlock(block *dcrutil.Block, blockHeight int64) []SKAEmissionRecord {
	var emissions []SKAEmissionRecord

	for _, tx := range block.Transactions() {
		msgTx := tx.MsgTx()

		// Check if this is an SKA emission transaction
		if wire.IsSKAEmissionTransaction(msgTx) {
			// Extract coin types and create emission records
			seenCoinTypes := make(map[cointype.CoinType]bool)

			for _, txOut := range msgTx.TxOut {
				if txOut.CoinType.IsSKA() && !seenCoinTypes[txOut.CoinType] {
					// Extract nonce from the transaction's authorization
					var nonce uint64 = 1 // Default nonce for first emission

					// Try to extract authorization for nonce
					if len(msgTx.TxIn) > 0 && len(msgTx.TxIn[0].SignatureScript) > 0 {
						auth, err := extractEmissionAuthorization(msgTx.TxIn[0].SignatureScript)
						if err == nil && auth != nil {
							nonce = auth.Nonce
						}
					}

					emissions = append(emissions, SKAEmissionRecord{
						CoinType: txOut.CoinType,
						Nonce:    nonce,
						Height:   blockHeight,
						TxHash:   *tx.Hash(),
					})

					seenCoinTypes[txOut.CoinType] = true
				}
			}
		}
	}

	return emissions
}
