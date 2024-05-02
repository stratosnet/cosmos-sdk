package v4

import (
	"fmt"
	"sort"

	sdkmath "cosmossdk.io/math"
	"github.com/cosmos/cosmos-sdk/codec"
	storetypes "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/staking/exported"
	"github.com/cosmos/cosmos-sdk/x/staking/types"
)

// MigrateStore performs in-place store migrations from v3 to v4.
func MigrateStore(ctx sdk.Context, storeKey storetypes.StoreKey, cdc codec.BinaryCodec, legacySubspace exported.Subspace) error {
	store := ctx.KVStore(storeKey)

	// migrate params
	if err := migrateParams(ctx, store, cdc, legacySubspace); err != nil {
		return err
	}

	// migrate unbonding delegations
	if err := migrateUBDEntries(ctx, store, cdc, legacySubspace); err != nil {
		return err
	}

	// migrate validator power store
	if err := migrateValidatorsPowerStore(ctx, store, cdc); err != nil {
		return err
	}

	return nil
}

// migrateParams will set the params to store from legacySubspace
func migrateParams(ctx sdk.Context, store storetypes.KVStore, cdc codec.BinaryCodec, legacySubspace exported.Subspace) error {
	var legacyParams types.Params
	legacySubspace.GetParamSet(ctx, &legacyParams)

	if err := legacyParams.Validate(); err != nil {
		return err
	}

	bz := cdc.MustMarshal(&legacyParams)
	store.Set(types.ParamsKey, bz)
	return nil
}

// migrateUBDEntries will remove the ubdEntries with same creation_height
// and create a new ubdEntry with updated balance and initial_balance
func migrateUBDEntries(ctx sdk.Context, store storetypes.KVStore, cdc codec.BinaryCodec, legacySubspace exported.Subspace) error {
	iterator := sdk.KVStorePrefixIterator(store, types.UnbondingDelegationKey)
	defer iterator.Close()

	for ; iterator.Valid(); iterator.Next() {
		ubd := types.MustUnmarshalUBD(cdc, iterator.Value())

		entriesAtSameCreationHeight := make(map[int64][]types.UnbondingDelegationEntry)
		for _, ubdEntry := range ubd.Entries {
			entriesAtSameCreationHeight[ubdEntry.CreationHeight] = append(entriesAtSameCreationHeight[ubdEntry.CreationHeight], ubdEntry)
		}

		creationHeights := make([]int64, 0, len(entriesAtSameCreationHeight))
		for k := range entriesAtSameCreationHeight {
			creationHeights = append(creationHeights, k)
		}

		sort.Slice(creationHeights, func(i, j int) bool { return creationHeights[i] < creationHeights[j] })

		ubd.Entries = make([]types.UnbondingDelegationEntry, 0, len(creationHeights))

		for _, h := range creationHeights {
			ubdEntry := types.UnbondingDelegationEntry{
				Balance:        sdk.ZeroInt(),
				InitialBalance: sdk.ZeroInt(),
			}
			for _, entry := range entriesAtSameCreationHeight[h] {
				ubdEntry.Balance = ubdEntry.Balance.Add(entry.Balance)
				ubdEntry.InitialBalance = ubdEntry.InitialBalance.Add(entry.InitialBalance)
				ubdEntry.CreationHeight = entry.CreationHeight
				ubdEntry.CompletionTime = entry.CompletionTime
			}
			ubd.Entries = append(ubd.Entries, ubdEntry)
		}

		// set the new ubd to the store
		setUBDToStore(ctx, store, cdc, ubd)
	}
	return nil
}

func setUBDToStore(ctx sdk.Context, store storetypes.KVStore, cdc codec.BinaryCodec, ubd types.UnbondingDelegation) {
	delegatorAddress := sdk.MustAccAddressFromBech32(ubd.DelegatorAddress)

	bz := types.MustMarshalUBD(cdc, ubd)

	addr, err := sdk.ValAddressFromBech32(ubd.ValidatorAddress)
	if err != nil {
		panic(err)
	}

	key := types.GetUBDKey(delegatorAddress, addr)

	store.Set(key, bz)
}

// FixValidatorByPowerIndexRecords because changing the sdk.DefaultPowerReduction at a height without properly update
// all ValidatorByPowerIndex records. now there are duplicated records for validator joins before the application
// of sdk.DefaultPowerReduction changes. If those validators become jailed now, jailed validator will be iterated in
// function ApplyAndReturnValidatorSetUpdates. this function fixed this issue by delete all ValidatorByPowerIndex
// records and add back only unjailed validator. this function should only be called once whenever
// sdk.DefaultPowerReduction changes
func migrateValidatorsPowerStore(ctx sdk.Context, store storetypes.KVStore, cdc codec.BinaryCodec) error {
	newPowerReduction := sdkmath.NewInt(1e15)

	// Iterate over validators, highest power to lowest.
	iterator := sdk.KVStoreReversePrefixIterator(store, types.ValidatorsByPowerIndexKey)
	defer iterator.Close()

	processed := make(map[string]int)
	processedIndex := 0
	count := 0
	for ; iterator.Valid(); iterator.Next() {
		// everything that is iterated in this loop is becoming or already a
		// part of the bonded validator set
		valAddr := sdk.ValAddress(iterator.Value())
		valBz := store.Get(types.GetValidatorKey(valAddr))
		if valBz == nil {
			panic(fmt.Sprintf("validator record not found for address: %X\n", valAddr))
		}
		validator := types.MustUnmarshalValidator(cdc, valBz)

		// delete all ValidatorByPowerIndex records
		key := iterator.Key()
		store.Delete(key)

		valAddrStr, err := sdk.Bech32ifyAddressBytes(sdk.GetConfig().GetBech32ValidatorAddrPrefix(), valAddr)
		if err != nil {
			count++
			return err
		}
		// add back ValidatorByPowerIndex that never handled
		if _, found := processed[valAddrStr]; !found {
			// jailed validators are not kept in the power index
			if validator.Jailed {
				continue
			}
			store.Set(types.GetValidatorsByPowerIndexKey(validator, newPowerReduction), validator.GetOperator())
			processed[valAddrStr] = processedIndex
			processedIndex++
		}
		count++
	}
	fmt.Println("total ValidatorByPowerIndex record: ", count)
	fmt.Println("valid ValidatorByPowerIndex record: ", processedIndex)
	return nil
}
