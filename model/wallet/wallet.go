// Package wallet models the data for a wallet
// It is not a complete wallet and only provides basic testing capabilities for rpc currently
package wallet

import (
	"crypto/rand"
	"io"
	"sync"

	"github.com/copernet/copernicus/conf"
	"github.com/copernet/copernicus/crypto"
	"github.com/copernet/copernicus/log"
	"github.com/copernet/copernicus/model/mempool"
	"github.com/copernet/copernicus/model/outpoint"
	"github.com/copernet/copernicus/model/script"
	"github.com/copernet/copernicus/model/tx"
	"github.com/copernet/copernicus/model/utxo"
	"github.com/copernet/copernicus/util"
	"github.com/copernet/copernicus/util/amount"
)

type Wallet struct {
	enable      bool
	broadcastTx bool

	reservedKeys []*crypto.PublicKey

	txnLock     *sync.RWMutex
	walletTxns  map[util.Hash]*WalletTx
	lockedCoins map[outpoint.OutPoint]struct{}
	payTxFee    *util.FeeRate

	crypto.KeyStore
	ScriptStore
	AddressBook
	WalletDB
}

var globalWallet *Wallet

/**
 * If fee estimation does not have enough data to provide estimates, use this
 * fee instead. Has no effect if not using fee estimation.
 * Override with -fallbackfee
 */
var fallbackFee = util.NewFeeRate(20000)

func InitWallet() {
	defer func() {
		if globalWallet == nil {
			globalWallet = &Wallet{enable: false}
		}
	}()

	if !conf.Cfg.Wallet.Enable {
		return
	}

	walletInstance := &Wallet{
		enable:      true,
		broadcastTx: conf.Cfg.Wallet.Broadcast,
		txnLock:     new(sync.RWMutex),
		walletTxns:  make(map[util.Hash]*WalletTx),
		lockedCoins: make(map[outpoint.OutPoint]struct{}),
		payTxFee:    util.NewFeeRate(0),
	}

	if err := walletInstance.Init(); err != nil {
		return
	}

	globalWallet = walletInstance
}

func GetInstance() *Wallet {
	return globalWallet
}

func (w *Wallet) Init() error {
	w.KeyStore.Init()
	w.ScriptStore.Init()
	w.AddressBook.Init()

	w.initDB()
	if err := w.loadFromDB(); err != nil {
		log.Error("Load wallet fail. error:" + err.Error())
		return err
	}
	return nil
}

func (w *Wallet) IsEnable() bool {
	return w.enable
}

func (w *Wallet) loadFromDB() error {
	secrets := w.loadSecrets()
	for _, secret := range secrets {
		privateKey := crypto.NewPrivateKeyFromBytes(secret, true)
		w.KeyStore.AddKey(privateKey)
	}

	scripts, err := w.loadScripts()
	if err != nil {
		return err
	}
	for _, sc := range scripts {
		w.ScriptStore.AddScript(sc)
	}

	addressBook, err := w.loadAddressBook()
	if err != nil {
		return err
	}
	for key, data := range addressBook {
		addressBookData := NewAddressBookData(data.Account, data.Purpose)
		w.AddressBook.SetAddressBook([]byte(key), addressBookData)
	}

	transactions, err := w.loadTransactions()
	if err != nil {
		return err
	}
	for _, wtx := range transactions {
		w.walletTxns[wtx.Tx.GetHash()] = wtx
	}
	log.Info("load wallet from db successfully. keys:%v, scripts:%v, addressbook:%v, txns:%v",
		len(secrets), len(scripts), len(addressBook), len(transactions))
	return nil
}

func (w *Wallet) GenerateNewKey() (*crypto.PublicKey, error) {
	secret := make([]byte, 32)
	io.ReadFull(rand.Reader, secret)
	privateKey := crypto.NewPrivateKeyFromBytes(secret, true)
	w.AddKey(privateKey)
	err := w.saveSecret(secret)
	if err != nil {
		log.Error("GenerateNewKey save to db fail. error:%s", err.Error())
		return nil, err
	}
	return privateKey.PubKey(), nil
}

func (w *Wallet) GetReservedKey() (*crypto.PublicKey, error) {
	// wallet function is only for testing. The keypool is not supported yet.
	// generate new key each time
	reservedKey, err := w.GenerateNewKey()
	if err != nil {
		return nil, err
	}
	w.reservedKeys = append(w.reservedKeys, reservedKey)
	return reservedKey, nil
}

func (w *Wallet) AddScript(s *script.Script) error {
	w.ScriptStore.AddScript(s)
	err := w.saveScript(s)
	if err != nil {
		log.Error("AddScript save to db fail. error:%s", err.Error())
		return err
	}
	return nil
}

func (w *Wallet) SetAddressBook(keyHash []byte, account string, purpose string) error {
	addressBookData := NewAddressBookData(account, purpose)
	w.AddressBook.SetAddressBook(keyHash, addressBookData)
	err := w.saveAddressBook(keyHash, addressBookData)
	if err != nil {
		log.Error("SetAddressBook save to db fail. error:%s", err.Error())
		return err
	}
	return nil
}

func (w *Wallet) AddToWallet(txn *tx.Tx, extInfo map[string]string) error {
	log.Error("rpc AddToWallet")

	if extInfo == nil {
		extInfo = make(map[string]string)
	}
	walletTx := NewWalletTx(txn, extInfo, true, "")
	txHash := txn.GetHash()

	w.txnLock.Lock()
	defer w.txnLock.Unlock()
	if _, ok := w.walletTxns[txHash]; !ok {
		w.walletTxns[txHash] = walletTx
	}

	err := w.saveWalletTx(&txHash, walletTx)
	if err != nil {
		log.Error("AddToWallet save to db fail. error:%s", err.Error())
		return err
	}
	return nil
}

func (w *Wallet) GetWalletTxns() []*WalletTx {
	walletTxns := make([]*WalletTx, 0, len(w.walletTxns))

	w.txnLock.RLock()
	defer w.txnLock.RUnlock()

	for _, walletTx := range w.walletTxns {
		walletTxns = append(walletTxns, walletTx)
	}
	return walletTxns
}

func (w *Wallet) IsTrusted(walletTx *WalletTx) bool {
	// Quick answer in most cases
	if !walletTx.CheckFinalForForCurrentBlock() {
		return false
	}

	depth := walletTx.GetDepthInMainChain()
	if depth >= 1 {
		return true
	}

	// Don't trust unconfirmed transactions from us unless they are in the
	// mempool.
	if !mempool.GetInstance().IsTransactionInPool(walletTx.Tx) {
		return false
	}

	w.txnLock.RLock()
	defer w.txnLock.RUnlock()

	// Trusted if all inputs are from us and are in the mempool:
	for _, txIn := range walletTx.Tx.GetIns() {
		// Transactions not sent by us: not trusted
		prevTxn, ok := w.walletTxns[txIn.PreviousOutPoint.Hash]
		if !ok {
			return false
		}
		prevOut := prevTxn.Tx.GetTxOut(int(txIn.PreviousOutPoint.Index))
		if !IsUnlockable(prevOut.GetScriptPubKey()) {
			return false
		}
	}

	return true
}

func (w *Wallet) GetBalance() amount.Amount {
	balance := amount.Amount(0)

	w.txnLock.RLock()
	defer w.txnLock.RUnlock()

	for _, walletTx := range w.walletTxns {
		if w.IsTrusted(walletTx) {
			balance += walletTx.GetAvailableCredit(true)
		}
	}
	return balance
}

func (w *Wallet) GetBroadcastTx() bool {
	return w.broadcastTx
}

func (w *Wallet) SetBroadcastTx(broadcastTx bool) {
	w.broadcastTx = broadcastTx
}

func (w *Wallet) SetFeeRate(feePaid int64, byteSize int64) {
	w.payTxFee = util.NewFeeRateWithSize(feePaid, byteSize)
}

func (w *Wallet) GetMinimumFee(byteSize int) int64 {
	feeNeeded := w.payTxFee.GetFee(byteSize)
	// User didn't set tx fee
	if feeNeeded == 0 {
		minFeeRate := mempool.GetInstance().GetMinFeeRate()
		feeNeeded = minFeeRate.GetFee(byteSize)

		// ... unless we don't have enough mempool data for estimatefee, then
		// use fallbackFee.
		if feeNeeded == 0 {
			feeNeeded = fallbackFee.GetFee(byteSize)
		}
	}

	// Prevent user from paying a fee below minRelayTxFee or minTxFee.
	cfgMinFeeRate := util.NewFeeRate(conf.Cfg.Mempool.MinFeeRate)
	feeNeeded = util.MaxI(feeNeeded, cfgMinFeeRate.GetFee(byteSize))

	// But always obey the maximum.
	feeNeeded = util.MinI(feeNeeded, util.MaxFee)

	return feeNeeded
}

func (w *Wallet) GetUnspentCoin(outPoint *outpoint.OutPoint) *utxo.Coin {
	w.txnLock.RLock()
	defer w.txnLock.RUnlock()
	if wtx, ok := w.walletTxns[outPoint.Hash]; ok {
		return wtx.GetUnspentCoin(int(outPoint.Index))
	}
	return nil
}

func (w *Wallet) MarkSpent(outPoint *outpoint.OutPoint) {
	w.txnLock.RLock()
	defer w.txnLock.RUnlock()
	if wtx, ok := w.walletTxns[outPoint.Hash]; ok {
		wtx.MarkSpent(int(outPoint.Index))
	}
}

func IsUnlockable(scriptPubKey *script.Script) bool {
	if globalWallet == nil || scriptPubKey == nil {
		return false
	}

	pubKeyType, pubKeys, isStandard := scriptPubKey.IsStandardScriptPubKey()
	if !isStandard || pubKeyType == script.ScriptNonStandard || pubKeyType == script.ScriptNullData {
		return false
	}

	if pubKeyType == script.ScriptHash {
		redeemScript := globalWallet.GetScript(pubKeys[0])
		if redeemScript == nil {
			return false
		}
		pubKeyType, pubKeys, isStandard = redeemScript.IsStandardScriptPubKey()
		if !isStandard || pubKeyType == script.ScriptNonStandard || pubKeyType == script.ScriptNullData {
			return false
		}
	}

	if pubKeyType == script.ScriptPubkey {
		pubKeyHash := util.Hash160(pubKeys[0])
		return globalWallet.GetKeyPair(pubKeyHash) != nil

	} else if pubKeyType == script.ScriptPubkeyHash {
		return globalWallet.GetKeyPair(pubKeys[0]) != nil

	} else if pubKeyType == script.ScriptMultiSig {
		// Only consider transactions "mine" if we own ALL the keys
		// involved. Multi-signature transactions that are partially owned
		// (somebody else has a key that can spend them) enable
		// spend-out-from-under-you attacks, especially in shared-wallet
		// situations.
		for _, pubKey := range pubKeys[1:] {
			pubKeyHash := util.Hash160(pubKey)
			if globalWallet.GetKeyPair(pubKeyHash) == nil {
				return false
			}
		}
		return true
	}
	return false
}