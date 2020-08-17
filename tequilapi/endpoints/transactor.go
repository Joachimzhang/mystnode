/*
 * Copyright (C) 2019 The "MysteriumNetwork/node" Authors.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package endpoints

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/julienschmidt/httprouter"
	"github.com/mysteriumnetwork/node/tequilapi/contract"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"github.com/vcraescu/go-paginator"
	"github.com/vcraescu/go-paginator/adapter"

	"github.com/mysteriumnetwork/node/identity"
	"github.com/mysteriumnetwork/node/identity/registry"
	"github.com/mysteriumnetwork/node/session/pingpong"
	"github.com/mysteriumnetwork/node/tequilapi/client"
	"github.com/mysteriumnetwork/node/tequilapi/utils"
)

// Transactor represents interface to Transactor service
type Transactor interface {
	FetchRegistrationFees() (registry.FeesResponse, error)
	FetchSettleFees() (registry.FeesResponse, error)
	TopUp(identity string) error
	RegisterIdentity(identity string, regReqDTO *registry.IdentityRegistrationRequestDTO) error
}

// promiseSettler settles the given promises
type promiseSettler interface {
	ForceSettle(providerID identity.Identity, accountantID common.Address) error
	SettleWithBeneficiary(id identity.Identity, beneficiary, accountantID common.Address) error
	GetAccountantFee() (uint16, error)
}

type settlementHistoryProvider interface {
	Query(*pingpong.SettlementHistoryQuery) error
}

type transactorEndpoint struct {
	transactor                Transactor
	promiseSettler            promiseSettler
	settlementHistoryProvider settlementHistoryProvider
}

// NewTransactorEndpoint creates and returns transactor endpoint
func NewTransactorEndpoint(transactor Transactor, promiseSettler promiseSettler, settlementHistoryProvider settlementHistoryProvider) *transactorEndpoint {
	return &transactorEndpoint{
		transactor:                transactor,
		promiseSettler:            promiseSettler,
		settlementHistoryProvider: settlementHistoryProvider,
	}
}

// Fees represents the transactor fees
// swagger:model Fees
type Fees struct {
	Registration uint64 `json:"registration"`
	Settlement   uint64 `json:"settlement"`
	Accountant   uint16 `json:"accountant"`
}

// swagger:operation GET /transactor/fees Fees
// ---
// summary: Returns fees
// description: Returns fees applied by Transactor
// responses:
//   200:
//     description: fees applied by Transactor
//     schema:
//       "$ref": "#/definitions/Fees"
//   500:
//     description: Internal server error
//     schema:
//       "$ref": "#/definitions/ErrorMessageDTO"
func (te *transactorEndpoint) TransactorFees(resp http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	registrationFees, err := te.transactor.FetchRegistrationFees()
	if err != nil {
		utils.SendError(resp, err, http.StatusInternalServerError)
		return
	}
	settlementFees, err := te.transactor.FetchSettleFees()
	if err != nil {
		utils.SendError(resp, err, http.StatusInternalServerError)
		return
	}
	accountantFees, err := te.promiseSettler.GetAccountantFee()
	if err != nil {
		utils.SendError(resp, err, http.StatusInternalServerError)
		return
	}

	f := Fees{
		Registration: registrationFees.Fee,
		Settlement:   settlementFees.Fee,
		Accountant:   accountantFees,
	}

	utils.WriteAsJSON(f, resp)
}

// SettleRequest represents the request to settle accountant promises
// swagger:model SettleRequest
type SettleRequest struct {
	AccountantID string `json:"accountant_id"`
	ProviderID   string `json:"provider_id"`
}

// swagger:operation POST /transactor/settle/sync SettleSync
// ---
// summary: forces the settlement of promises for the given provider and accountant
// description: Forces a settlement for the accountant promises and blocks until the settlement is complete.
// parameters:
// - in: body
//   name: body
//   description: settle request body
//   schema:
//     $ref: "#/definitions/SettleRequest"
// responses:
//   202:
//     description: settle request accepted
//   500:
//     description: Internal server error
//     schema:
//       "$ref": "#/definitions/ErrorMessageDTO"
func (te *transactorEndpoint) SettleSync(resp http.ResponseWriter, request *http.Request, _ httprouter.Params) {
	err := te.settle(request, te.promiseSettler.ForceSettle)
	if err != nil {
		utils.SendError(resp, err, http.StatusInternalServerError)
		return
	}

	resp.WriteHeader(http.StatusOK)
}

// swagger:operation POST /transactor/settle/async SettleAsync
// ---
// summary: forces the settlement of promises for the given provider and accountant
// description: Forces a settlement for the accountant promises. Does not wait for completion.
// parameters:
// - in: body
//   name: body
//   description: settle request body
//   schema:
//     $ref: "#/definitions/SettleRequest"
// responses:
//   202:
//     description: settle request accepted
//   500:
//     description: Internal server error
//     schema:
//       "$ref": "#/definitions/ErrorMessageDTO"
func (te *transactorEndpoint) SettleAsync(resp http.ResponseWriter, request *http.Request, _ httprouter.Params) {
	err := te.settle(request, func(provider identity.Identity, accountant common.Address) error {
		go func() {
			err := te.promiseSettler.ForceSettle(provider, accountant)
			if err != nil {
				log.Error().Err(err).Msgf("could not settle provider(%q) promises", provider.Address)
			}
		}()
		return nil
	})
	if err != nil {
		utils.SendError(resp, err, http.StatusInternalServerError)
		return
	}

	resp.WriteHeader(http.StatusAccepted)
}

func (te *transactorEndpoint) settle(request *http.Request, settler func(identity.Identity, common.Address) error) error {
	req := SettleRequest{}

	err := json.NewDecoder(request.Body).Decode(&req)
	if err != nil {
		return errors.Wrap(err, "failed to unmarshal settle request")
	}

	return errors.Wrap(settler(identity.FromAddress(req.ProviderID), common.HexToAddress(req.AccountantID)), "settling failed")
}

// swagger:operation POST /transactor/topup
// ---
// summary: tops up myst to the given identity
// description: tops up myst to the given identity
// parameters:
// - in: body
//   name: body
//   description: top up request body
//   schema:
//     $ref: "#/definitions/TopUpRequestDTO"
// responses:
//   202:
//     description: top up request accepted
//   500:
//     description: Internal server error
//     schema:
//       "$ref": "#/definitions/ErrorMessageDTO"
//   400:
//     description: Bad request
//     schema:
//       "$ref": "#/definitions/ErrorMessageDTO"
func (te *transactorEndpoint) TopUp(resp http.ResponseWriter, request *http.Request, _ httprouter.Params) {
	topUpDTO := registry.TopUpRequest{}

	err := json.NewDecoder(request.Body).Decode(&topUpDTO)
	if err != nil {
		utils.SendError(resp, errors.Wrap(err, "failed to parse top up request"), http.StatusBadRequest)
		return
	}

	err = te.transactor.TopUp(topUpDTO.Identity)
	if err != nil {
		utils.SendError(resp, err, http.StatusInternalServerError)
		return
	}

	resp.WriteHeader(http.StatusAccepted)
}

// swagger:operation POST /identities/{id}/register Identity RegisterIdentity
// ---
// summary: Registers identity
// description: Registers identity on Mysterium Network smart contracts using Transactor
// parameters:
// - name: id
//   in: path
//   description: Identity address to register
//   type: string
//   required: true
// - in: body
//   name: body
//   description: all body parameters a optional
//   schema:
//     $ref: "#/definitions/IdentityRegistrationRequestDTO"
// responses:
//   200:
//     description: Payout info registered
//   400:
//     description: Bad request
//     schema:
//       "$ref": "#/definitions/ErrorMessageDTO"
//   500:
//     description: Internal server error
//     schema:
//       "$ref": "#/definitions/ErrorMessageDTO"
func (te *transactorEndpoint) RegisterIdentity(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
	identity := params.ByName("id")

	regReqDTO := &registry.IdentityRegistrationRequestDTO{}

	err := json.NewDecoder(request.Body).Decode(&regReqDTO)
	if err != nil {
		utils.SendError(resp, errors.Wrap(err, "failed to parse identity registration request"), http.StatusBadRequest)
		return
	}

	err = te.transactor.RegisterIdentity(identity, regReqDTO)
	if err != nil {
		log.Err(err).Msgf("Failed identity registration request for ID: %s, %+v", identity, regReqDTO)
		utils.SendError(resp, errors.Wrap(err, "failed identity registration request"), http.StatusInternalServerError)
		return
	}

	resp.WriteHeader(http.StatusAccepted)
}

func (te *transactorEndpoint) SetBeneficiary(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
	id := params.ByName("id")

	req := &client.SettleWithBeneficiaryRequest{}
	err := json.NewDecoder(request.Body).Decode(&req)
	if err != nil {
		utils.SendError(resp, fmt.Errorf("failed to parse set beneficiary request: %w", err), http.StatusBadRequest)
		return
	}

	err = te.promiseSettler.SettleWithBeneficiary(identity.FromAddress(id), common.HexToAddress(req.Beneficiary), common.HexToAddress(req.AccountantID))
	if err != nil {
		log.Err(err).Msgf("Failed set beneficiary request for ID: %s, %+v", id, req)
		utils.SendError(resp, fmt.Errorf("failed set beneficiary request: %w", err), http.StatusInternalServerError)
		return
	}

	resp.WriteHeader(http.StatusAccepted)
}

// swagger:operation GET /settle/history SettlementHistory
// ---
// summary: Returns settlement history
// description: Returns settlement history
// parameters:
//   - in: query
//     name: at_from
//     description: To filter the settlements from this date. Formatted in RFC3339 e.g. 2020-07-01T00:00:00Z.
//     type: string
//   - in: query
//     name: at_to
//     description: To filter the settlements until this date. Formatted in RFC3339 e.g. 2020-07-01T00:00:00Z.
//     type: string
//   - in: query
//     name: provider_id
//     description: Provider ID to filter the settlements by.
//     type: string
//   - in: query
//     name: accountant_id
//     description: Accountant ID to filter the sessions by.
//     type: string
// responses:
//   200:
//     description: Returns settlement history
//     schema:
//       "$ref": "#/definitions/ListSettlementsResponse"
//   400:
//     description: Bad request
//     schema:
//       "$ref": "#/definitions/ErrorMessageDTO"
//   500:
//     description: Internal server error
//     schema:
//       "$ref": "#/definitions/ErrorMessageDTO"
func (te *transactorEndpoint) SettlementHistory(resp http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	query := pingpong.NewSettlementHistoryQuery()

	from := time.Now().AddDate(0, 0, -30)
	if fromStr := req.URL.Query().Get("at_from"); fromStr != "" {
		var err error
		if from, err = time.Parse(time.RFC3339, fromStr); err != nil {
			utils.SendError(resp, err, http.StatusBadRequest)
			return
		}
	}
	query.FilterFrom(from)

	to := time.Now()
	if toStr := req.URL.Query().Get("at_to"); toStr != "" {
		var err error
		if to, err = time.Parse(time.RFC3339, toStr); err != nil {
			utils.SendError(resp, err, http.StatusBadRequest)
			return
		}
	}
	query.FilterTo(to)

	if providerID := req.URL.Query().Get("provider_id"); providerID != "" {
		query.FilterProviderID(identity.FromAddress(providerID))
	}
	if accountantID := req.URL.Query().Get("accountant_id"); accountantID != "" {
		query.FilterAccountantID(common.HexToAddress(accountantID))
	}

	page := 1
	if pageStr := req.URL.Query().Get("page"); pageStr != "" {
		var err error
		if page, err = strconv.Atoi(pageStr); err != nil {
			utils.SendError(resp, err, http.StatusBadRequest)
			return
		}
	}

	pageSize := 50
	if pageSizeStr := req.URL.Query().Get("page_size"); pageSizeStr != "" {
		var err error
		if pageSize, err = strconv.Atoi(pageSizeStr); err != nil {
			utils.SendError(resp, err, http.StatusBadRequest)
			return
		}
	}

	if err := te.settlementHistoryProvider.Query(query.FetchEntries()); err != nil {
		utils.SendError(resp, err, http.StatusInternalServerError)
		return
	}

	var settlements []pingpong.SettlementHistoryEntry
	p := paginator.New(adapter.NewSliceAdapter(query.Entries), pageSize)
	p.SetPage(page)
	if err := p.Results(&settlements); err != nil {
		utils.SendError(resp, err, http.StatusInternalServerError)
		return
	}

	response := contract.NewSettlementListResponse(settlements, &p)
	utils.WriteAsJSON(response, resp)
}

// AddRoutesForTransactor attaches Transactor endpoints to router
func AddRoutesForTransactor(router *httprouter.Router, transactor Transactor, promiseSettler promiseSettler, settlementHistoryProvider settlementHistoryProvider) {
	te := NewTransactorEndpoint(transactor, promiseSettler, settlementHistoryProvider)
	router.POST("/identities/:id/register", te.RegisterIdentity)
	router.POST("/identities/:id/beneficiary", te.SetBeneficiary)
	router.GET("/transactor/fees", te.TransactorFees)
	router.POST("/transactor/topup", te.TopUp)
	router.POST("/transactor/settle/sync", te.SettleSync)
	router.POST("/transactor/settle/async", te.SettleAsync)
	router.GET("/transactor/settle/history", te.SettlementHistory)
}
