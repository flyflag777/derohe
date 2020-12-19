// Copyright 2017-2021 DERO Project. All rights reserved.
// Use of this source code in any form is governed by RESEARCH license.
// license can be found in the LICENSE file.
// GPG: 0F39 E425 8C65 3947 702A  8234 08B2 0360 A03A 9DE8
//
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND ANY
// EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED WARRANTIES OF
// MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL
// THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO,
// PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
// INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT,
// STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF
// THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package blockchain

//import "fmt"
import "time"

/*import "bytes"
import "encoding/binary"

import "github.com/romana/rlog"

*/

import "sync"
import "runtime/debug"
import "github.com/deroproject/graviton"

//import "github.com/romana/rlog"
import log "github.com/sirupsen/logrus"

//import "github.com/deroproject/derohe/config"
import "github.com/deroproject/derohe/block"
import "github.com/deroproject/derohe/crypto"
import "github.com/deroproject/derohe/transaction"
import "github.com/deroproject/derohe/crypto/bn256"

//import "github.com/deroproject/derosuite/emission"

// caches x of transactions validity
// it is always atomic
// the cache is not txhash -> validity mapping
// instead it is txhash+expanded ringmembers
// if the entry exist, the tx is valid
// it stores special hash and first seen time
// this can only be used on expanded transactions
var transaction_valid_cache sync.Map

// this go routine continuously scans and cleans up the cache for expired entries
func clean_up_valid_cache() {

	for {
		time.Sleep(3600 * time.Second)
		current_time := time.Now()

		// track propagation upto 10 minutes
		transaction_valid_cache.Range(func(k, value interface{}) bool {
			first_seen := value.(time.Time)
			if current_time.Sub(first_seen).Round(time.Second).Seconds() > 3600 {
				transaction_valid_cache.Delete(k)
			}
			return true
		})

	}
}

/* Coinbase transactions need to verify registration
 * */
func (chain *Blockchain) Verify_Transaction_Coinbase(cbl *block.Complete_Block, minertx *transaction.Transaction) (result bool) {

	if !minertx.IsCoinbase() { // transaction is not coinbase, return failed
		return false
	}

	// make sure miner address is registered

	_, topos := chain.Store.Topo_store.binarySearchHeight(int64(cbl.Bl.Height - 1))
	// load all db versions one by one and check whether the root hash matches the one mentioned in the tx
	if len(topos) < 1 {
		return false
	}

	var balance_tree *graviton.Tree
	for i := range topos {

		toporecord, err := chain.Store.Topo_store.Read(topos[i])
		if err != nil {
			log.Infof("Skipping block at height %d due to error while obtaining toporecord %s\n", i, err)
			continue
		}

		ss, err := chain.Store.Balance_store.LoadSnapshot(toporecord.State_Version)
		if err != nil {
			panic(err)
		}

		if balance_tree, err = ss.GetTree(BALANCE_TREE); err != nil {
			panic(err)
		}

		if _, err := balance_tree.Get(minertx.MinerAddress[:]); err != nil {
			//logger.Infof("balance not obtained err %s\n",err)
			return false
		} else {
			return true
		}

	}

	return true
}

// all non miner tx must be non-coinbase tx
// each check is placed in a separate  block of code, to avoid ambigous code or faulty checks
// all check are placed and not within individual functions ( so as we cannot skip a check )
// This function verifies tx fully, means all checks,
// if the transaction has passed the check it can be added to mempool, relayed or added to blockchain
// the transaction has already been deserialized thats it
// It also expands the transactions, using the repective state trie
func (chain *Blockchain) Verify_Transaction_NonCoinbase(hf_version int64, tx *transaction.Transaction) (result bool) {

	var tx_hash crypto.Hash
	defer func() { // safety so if anything wrong happens, verification fails
		if r := recover(); r != nil {
			logger.WithFields(log.Fields{"txid": tx_hash}).Warnf("Recovered while Verifying transaction, failed verification, Stack trace below")
			logger.Warnf("Stack trace  \n%s", debug.Stack())
			result = false
		}
	}()

	if tx.Version != 1 {
		return false
	}

	if tx.TransactionType == transaction.REGISTRATION {
		if tx.IsRegistrationValid() {
			return true
		}
		return false
	}

	// currently we allow 2 types of transaction
	if !(tx.TransactionType == transaction.NORMAL || tx.TransactionType == transaction.REGISTRATION) {
		return false
	}

	// check sanity
	if tx.Statement.RingSize != uint64(len(tx.Statement.Publickeylist_compressed)) || tx.Statement.RingSize != uint64(len(tx.Statement.Publickeylist)) {
		return false
	}

	// avoid some bugs lurking elsewhere
	if tx.Height != uint64(int64(tx.Height)) {
		return false
	}

	if tx.Statement.RingSize < 2 { // ring size minimum 4
		return false
	}

	if tx.Statement.RingSize > 128 { // ring size current limited to 128
		return false
	}

	if !crypto.IsPowerOf2(len(tx.Statement.Publickeylist_compressed)) {
		return false
	}

	// check duplicate ring members within the tx
	{
		key_map := map[string]bool{}
		for i := range tx.Statement.Publickeylist_compressed {
			key_map[string(tx.Statement.Publickeylist_compressed[i][:])] = true
		}
		if len(key_map) != len(tx.Statement.Publickeylist_compressed) {
			return false
		}

	}

	var balance_tree *graviton.Tree

	tx.Statement.CLn = tx.Statement.CLn[:0]
	tx.Statement.CRn = tx.Statement.CRn[:0]

	// this expansion needs  balance state
	if len(tx.Statement.CLn) == 0 { // transaction needs to be expanded
		_, topos := chain.Store.Topo_store.binarySearchHeight(int64(tx.Height))

		// load all db versions one by one and check whether the root hash matches the one mentioned in the tx
		if len(topos) < 1 {
			panic("TX could NOT be expanded")
		}

		for i := range topos {
			toporecord, err := chain.Store.Topo_store.Read(topos[i])
			if err != nil {
				//log.Infof("Skipping block at height %d due to error while obtaining toporecord %s\n", i, err)
				continue
			}

			ss, err := chain.Store.Balance_store.LoadSnapshot(toporecord.State_Version)
			if err != nil {
				panic(err)
			}

			if balance_tree, err = ss.GetTree(BALANCE_TREE); err != nil {
				panic(err)
			}

			if hash, err := balance_tree.Hash(); err != nil {
				panic(err)
			} else {
				//logger.Infof("dTX balance tree hash from tx %x  treehash from blockchain  %x", tx.Statement.Roothash, hash)

				if hash == tx.Statement.Roothash {
					break // we have found the balance tree with which it was built now lets verify
				}
			}
			balance_tree = nil
		}
	}

	if balance_tree == nil {
		panic("mentioned balance tree not found, cannot verify TX")
	}

	//logger.Infof("dTX  state tree has been found")

	// now lets calculate CLn and CRn
	for i := range tx.Statement.Publickeylist_compressed {
		balance_serialized, err := balance_tree.Get(tx.Statement.Publickeylist_compressed[i][:])
		if err != nil {
			//logger.Infof("balance not obtained err %s\n",err)
			return false
		}

		var ll, rr bn256.G1
		ebalance := new(crypto.ElGamal).Deserialize(balance_serialized)

		ll.Add(ebalance.Left, tx.Statement.C[i])
		tx.Statement.CLn = append(tx.Statement.CLn, &ll)
		rr.Add(ebalance.Right, tx.Statement.D)
		tx.Statement.CRn = append(tx.Statement.CRn, &rr)
	}

	if tx.Proof.Verify(&tx.Statement, tx.GetHash()) {
		//logger.Infof("dTX verified with proof successfuly")
		return true
	}
	logger.Infof("transaction verification failed\n")

	return false

	/*
		var tx_hash crypto.Hash
		var tx_serialized []byte // serialized tx
		defer func() {           // safety so if anything wrong happens, verification fails
			if r := recover(); r != nil {
				logger.WithFields(log.Fields{"txid": tx_hash}).Warnf("Recovered while Verifying transaction, failed verification, Stack trace below")
				logger.Warnf("Stack trace  \n%s", debug.Stack())
				result = false
			}
		}()

		tx_hash = tx.GetHash()

		if tx.Version != 2 {
			return false
		}

		// make sure atleast 1 vin and 1 vout are there
		if len(tx.Vin) < 1 || len(tx.Vout) < 1 {
			logger.WithFields(log.Fields{"txid": tx_hash}).Warnf("Incoming TX does NOT have atleast 1 vin and 1 vout")
			return false
		}

		// this means some other checks have failed somewhere else
		if tx.IsCoinbase() { // transaction coinbase must never come here
			logger.WithFields(log.Fields{"txid": tx_hash}).Warnf("Coinbase tx in non coinbase path, Please investigate")
			return false
		}

		// Vin can be only specific type rest all make the fail case
		for i := 0; i < len(tx.Vin); i++ {
			switch tx.Vin[i].(type) {
			case transaction.Txin_gen:
				return false // this is for coinbase so fail it
			case transaction.Txin_to_key: // pass
			default:
				return false
			}
		}

		if hf_version >= 2 {
			if len(tx.Vout) >= config.MAX_VOUT {
				rlog.Warnf("Tx %s has more Vouts than allowed limit 7 actual %d", tx_hash, len(tx.Vout))
				return
			}
		}

		// Vout can be only specific type rest all make th fail case
		for i := 0; i < len(tx.Vout); i++ {
			switch tx.Vout[i].Target.(type) {
			case transaction.Txout_to_key: // pass

				public_key := tx.Vout[i].Target.(transaction.Txout_to_key).Key

				if !public_key.Public_Key_Valid() { // if public_key is not valid ( not a point on the curve reject the TX)
					logger.WithFields(log.Fields{"txid": tx_hash}).Warnf("TX public is INVALID %s ", public_key)
					return false

				}
			default:
				return false
			}
		}

		// Vout should have amount 0
		for i := 0; i < len(tx.Vout); i++ {
			if tx.Vout[i].Amount != 0 {
				logger.WithFields(log.Fields{"txid": tx_hash, "Amount": tx.Vout[i].Amount}).Warnf("Amount must be zero in ringCT world")
				return false
			}
		}

		// check the mixin , it should be atleast 4 and should be same through out the tx ( all other inputs)
		// someone did send a mixin of 3 in 12006 block height
		// atlantis has minimum mixin of 5
		if hf_version >= 2 {
			mixin := len(tx.Vin[0].(transaction.Txin_to_key).Key_offsets)

			if mixin < config.MIN_MIXIN {
				logger.WithFields(log.Fields{"txid": tx_hash, "Mixin": mixin}).Warnf("Mixin cannot be more than %d.", config.MIN_MIXIN)
				return false
			}
			if mixin >= config.MAX_MIXIN {
				logger.WithFields(log.Fields{"txid": tx_hash, "Mixin": mixin}).Warnf("Mixin cannot be more than %d.", config.MAX_MIXIN)
				return false
			}

			for i := 0; i < len(tx.Vin); i++ {
				if mixin != len(tx.Vin[i].(transaction.Txin_to_key).Key_offsets) {
					logger.WithFields(log.Fields{"txid": tx_hash, "Mixin": mixin}).Warnf("Mixin must be same for entire TX in ringCT world")
					return false
				}
			}
		}

		// duplicate ringmembers are not allowed, check them here
		// just in case protect ourselves as much as we can
		for i := 0; i < len(tx.Vin); i++ {
			ring_members := map[uint64]bool{} // create a separate map for each input
			ring_member := uint64(0)
			for j := 0; j < len(tx.Vin[i].(transaction.Txin_to_key).Key_offsets); j++ {
				ring_member += tx.Vin[i].(transaction.Txin_to_key).Key_offsets[j]
				if _, ok := ring_members[ring_member]; ok {
					logger.WithFields(log.Fields{"txid": tx_hash, "input_index": i}).Warnf("Duplicate ring member within the TX")
					return false
				}
				ring_members[ring_member] = true // add member to ring member
			}

			//	rlog.Debugf("Ring members for %d %+v", i, ring_members )
		}

		// check whether the key image is duplicate within the inputs
		// NOTE: a block wide key_image duplication is done during block testing but we are still keeping it
		{
			kimages := map[crypto.Hash]bool{}
			for i := 0; i < len(tx.Vin); i++ {
				if _, ok := kimages[tx.Vin[i].(transaction.Txin_to_key).K_image]; ok {
					logger.WithFields(log.Fields{
						"txid":   tx_hash,
						"kimage": tx.Vin[i].(transaction.Txin_to_key).K_image,
					}).Warnf("TX using duplicate inputs within the TX")
					return false
				}
				kimages[tx.Vin[i].(transaction.Txin_to_key).K_image] = true // add element to map for next check
			}
		}

		// check whether the key image is low order attack, if yes reject it right now
		for i := 0; i < len(tx.Vin); i++ {
			k_image := crypto.Key(tx.Vin[i].(transaction.Txin_to_key).K_image)
			curve_order := crypto.CurveOrder()
			mult_result := crypto.ScalarMultKey(&k_image, &curve_order)
			if *mult_result != crypto.Identity {
				logger.WithFields(log.Fields{
					"txid":        tx_hash,
					"kimage":      tx.Vin[i].(transaction.Txin_to_key).K_image,
					"curve_order": curve_order,
					"mult_result": *mult_result,
					"identity":    crypto.Identity,
				}).Warnf("TX contains a low order key image attack, but we are already safeguarded")
				return false
			}
		}

		// disallow old transactions with borrowmean signatures
		if hf_version >= 2 {
			switch tx.RctSignature.Get_Sig_Type() {
			case ringct.RCTTypeSimple, ringct.RCTTypeFull:
				return false
			}
		}

		// check whether the TX contains a signature or NOT
		switch tx.RctSignature.Get_Sig_Type() {
		case ringct.RCTTypeSimpleBulletproof, ringct.RCTTypeSimple, ringct.RCTTypeFull: // default case, pass through
		default:
			logger.WithFields(log.Fields{"txid": tx_hash}).Warnf("TX does NOT contain a ringct signature. It is NOT possible")
			return false
		}

		// check tx size for validity
		if hf_version >= 2 {
			tx_serialized = tx.Serialize()
			if len(tx_serialized) >= config.CRYPTONOTE_MAX_TX_SIZE {
				rlog.Warnf("tx %s rejected Size(%d) is more than allowed(%d)", tx_hash, len(tx.Serialize()), config.CRYPTONOTE_MAX_TX_SIZE)
				return false
			}
		}

		// expand the signature first
		// whether the inputs are mature and can be used at time is verified while expanding the inputs

		//rlog.Debugf("txverify tx %s hf_version %d", tx_hash, hf_version )
		if !chain.Expand_Transaction_v2(dbtx, hf_version, tx) {
			rlog.Warnf("TX %s inputs could not be expanded or inputs are NOT mature", tx_hash)
			return false
		}

		//logger.Infof("Expanded tx %+v", tx.RctSignature)

		// create a temporary hash out of expanded transaction
		// this feature is very critical and helps the daemon by spreading out the compute load
		// over the entire time between 2 blocks
		// this tremendously helps in block propagation times
		// and make them easy to process just like like small 50 KB blocks

		// each ring member if 64 bytes
		tmp_buffer := make([]byte, 0, len(tx.Vin)*32+len(tx.Vin)*len(tx.Vin[0].(transaction.Txin_to_key).Key_offsets)*64)

		// build the buffer for special hash
		// DO NOT skip anything, use full serialized tx, it is used while building keccak hash
		// use everything from tx expansion etc
		for i := 0; i < len(tx.Vin); i++ { // append all mlsag sigs
			tmp_buffer = append(tmp_buffer, tx.RctSignature.MlsagSigs[i].II[0][:]...)
		}
		for i := 0; i < len(tx.RctSignature.MixRing); i++ {
			for j := 0; j < len(tx.RctSignature.MixRing[i]); j++ {
				tmp_buffer = append(tmp_buffer, tx.RctSignature.MixRing[i][j].Destination[:]...)
				tmp_buffer = append(tmp_buffer, tx.RctSignature.MixRing[i][j].Mask[:]...)
			}
		}

		// 1 less allocation this way
		special_hash := crypto.Keccak256(tx_serialized, tmp_buffer)

		if _, ok := transaction_valid_cache.Load(special_hash); ok {
			//logger.Infof("Found in cache %s ",tx_hash)
			return true
		} else {
			//logger.Infof("TX not found in cache %s len %d ",tx_hash, len(tmp_buffer))
		}

		// check the ring signature
		if !tx.RctSignature.Verify() {

			//logger.Infof("tx expanded %+v\n", tx.RctSignature.MixRing)
			logger.WithFields(log.Fields{"txid": tx_hash}).Warnf("TX RCT Signature failed")
			return false

		}

		// signature got verified, cache it
		transaction_valid_cache.Store(special_hash, time.Now())
		//logger.Infof("TX validity marked in cache %s ",tx_hash)

		//logger.WithFields(log.Fields{"txid": tx_hash}).Debugf("TX successfully verified")
	*/
	return true
}

// double spend check is separate from the core checks ( due to softforks )
func (chain *Blockchain) Verify_Transaction_NonCoinbase_DoubleSpend_Check(tx *transaction.Transaction) (result bool) {
	return true

}

// verify all non coinbase tx, single threaded for double spending on current active chain
func (chain *Blockchain) Verify_Block_DoubleSpending(cbl *block.Complete_Block) (result bool) {
	/*
		for i := 0; i < len(cbl.Txs); i++ {
			if !chain.Verify_Transaction_NonCoinbase_DoubleSpend_Check(dbtx, cbl.Txs[i]) {
				return false
			}
		}
	*/
	return true
}
