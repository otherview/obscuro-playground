package wallet_mock

import (
	"github.com/google/uuid"
	"simulation/common"
)

type Wallet struct {
	Address common.Address
}

func New() Wallet {
	//return Wallet{Address: uuid.New()}
	return Wallet{Address: uuid.New().ID()}
}
