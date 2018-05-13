package utxo

import (

	"github.com/btcboost/copernicus/model/outpoint"
	"github.com/btcboost/copernicus/model/utxo"
	"github.com/btcboost/copernicus/util"
)



func AccessByTxid(coinsCache *utxo.CoinsCache, hash *util.Hash) *utxo.Coin {
	out := outpoint.OutPoint{ *hash,  0}
	for int(out.Index) < 11000 { // todo modify to be precise
		alternate,_ := coinsCache.GetCoin(&out)
		if !alternate.IsSpent() {
			return alternate
		}
		out.Index++
	}
	return nil
}