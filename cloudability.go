package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"time"
)

type CloudabilityCostData struct {
	Limit      int         `json:"limit"`
	Meta       MetaSection `json:"meta"`
	Offset     int         `json:"offset"`
	Pagination struct {
		Next     string `json:"next"`
		Previous string `json:"previous"`
	} `json:"pagination"`
	Results      []ResultsEntry `json:"results"`
	TotalResults int            `json:"total_results"`
}

type ResultsEntry struct {
	AccountID      string `json:"vendor_account_identifier"`
	AccountName    string `json:"vendor_account_name"`
	CloudProvider  string `json:"vendor"`
	Cost           string `json:"unblended_cost"`
	CostCenter     string `json:"category4"`
	PayerAccountId string `json:"account_identifier"`
	UsageFamily    string `json:"usage_family"`
}

type MetaSection struct {
	Aggregates []AggregatesEntry `json:"aggregates"`
	Dates      struct {
		Start time.Time `json:"start"`
		End   time.Time `json:"end"`
	} `json:"dates"`
	Dimensions []Measure      `json:"dimensions"`
	Filters    []FiltersEntry `json:"filters"`
	Metrics    []Measure      `json:"metrics"`
}

type FiltersEntry struct {
	Comparator string `json:"comparator"`
	Value      string `json:"value"`
	Measure    `json:"measure"`
}

type AggregatesEntry struct {
	Value string `json:"value"`
	Element
}

type Measure struct {
	Element
	//Group       struct {
	//	ID   int    `json:"ID"`
	//	Key  string `json:"Key"`
	//	Name string `json:"Name"`
	//} `json:"group"`
	//SubGroup struct {
	//	ID   int    `json:"ID"`
	//	Key  string `json:"Key"`
	//	Name string `json:"Name"`
	//} `json:"sub_group"`
}

type Element struct {
	Description string `json:"description"`
	Label       string `json:"label"`
	Name        string `json:"name"`
	//DataType    string `json:"data_type"`
	//Type string `json:"type"`
}

func getCloudabilityData(configMap Configuration, options CommandLineOptions) *CloudabilityCostData {
	uri := "/v3/reporting/cost/run"

	cUrl, err := url.Parse(getMapKeyString(configMap, "api", "cloudability"))
	if err != nil {
		log.Fatalf("Error in Cloudability \"api_host\" value (%q): %v", configMap["api"], err)
	}

	now := time.Now()
	var startString, endString string
	if inTime, err := time.Parse("2006-01", *options.monthPtr); err == nil {
		if inTime.After(now) {
			log.Fatalf(
				"Error:  specified month, %q, is in the future.",
				*options.monthPtr,
			)
		}
		startString = inTime.Format("2006-01-02")
		endTime := inTime.AddDate(0, 1, -1)
		if endTime.After(now) {
			log.Printf(
				"Warning:  specified month, %q, extends into the future.",
				*options.monthPtr,
			)
			endTime = now
		}
		endString = endTime.Format("2006-01-02")
	} else {
		log.Fatalf("Error in Cloudability \"month\" value (%q): %v", *options.monthPtr, err)
	}

	costType := *options.costTypePtr
	if costType == "UnblendedCost" {
		costType = "unblended_cost"
	}

	qParams := cUrl.Query()
	qParams.Set("start_date", startString)
	qParams.Set("end_date", endString)
	qParams.Set("dimensions", "vendor,category4,account_identifier,vendor_account_name,vendor_account_identifier,usage_family")
	qParams.Set("metrics", costType)
	filtersAny := getMapKeyValue(configMap, "filters", "")
	if filters, ok := filtersAny.(map[any]any); ok {
		for filterAny, expAny := range filters {
			filter := getStringFromAny(filterAny, "Cloudability filter name")
			if expAny == nil {
				log.Fatalf("Missing value(s) for Cloudability filter %q", filter)
			}
			exp, ok := expAny.([]any)
			if !ok {
				log.Fatalf(
					"Unexpected value (%v) for Cloudability filter values for filter %q, expected an array of strings",
					expAny,
					filter,
				)
			}
			for _, valAny := range exp {
				val := getStringFromAny(valAny, "Cloudability filter value")
				qParams.Add("filters", filter+"=="+val)
			}
		}
	} else if filtersAny != nil {
		log.Fatalf("Error in Cloudability \"filters\" value (%q), type is %T, expected a mapping",
			filtersAny, filtersAny)
	}
	//qParams.Add("filters", "unblended_cost>0")
	qParams.Set("view_id", "0")
	qParams.Set("limit", "0")
	path, err := url.JoinPath(cUrl.Path, uri)
	if err != nil {
		log.Fatalf("Error composing Cloudability API path, joining %q to %q: %v", cUrl.Path, uri, err)
	}

	cUrl = &url.URL{
		Scheme:   "https",
		Host:     cUrl.Host,
		Path:     path,
		RawQuery: qParams.Encode(),
	}

	client := http.Client{Timeout: time.Second * 180}

	request, err := http.NewRequest("GET", cUrl.String(), http.NoBody)
	if err != nil {
		log.Fatalf("Error creating Cloudability request:  %v", err)
	}

	if _, ok := configMap["api_key"]; ok {
		apiKey := getMapKeyString(configMap, "api_key", "cloudability")
		request.SetBasicAuth(apiKey, "")
	} else {
		request.Header.Add("apptio-opentoken", getApptioOpentoken(configMap, client))
		environmentId := getMapKeyString(configMap, "environmentId", "cloudability")
		request.Header.Add("apptio-environmentid", environmentId)
	}
	request.Header.Add("Accept", "application/json")

	log.Println("[getCloudabilityData] Sending request for data")
	response, err := client.Do(request)
	if err != nil {
		log.Fatalf("Error sending request to Cloudability:  %v", err)
	}
	if response.StatusCode != http.StatusOK {
		log.Fatalf("Error getting data from Cloudability:  %d, %q", response.StatusCode, response.Status)
	}
	defer func(Body io.ReadCloser) {
		if err := Body.Close(); err != nil {
			log.Fatalf("Ignoring error closing Cloudability body: %v", err)
		}
	}(response.Body)
	responseBytes, err := io.ReadAll(response.Body)
	if err != nil {
		log.Fatalf("Error reading Cloudability response body: %v", err)
	}

	log.Println("[getCloudabilityData] Processing results")
	responseData := new(CloudabilityCostData)
	err = json.Unmarshal(responseBytes, responseData)
	if err != nil {
		log.Fatalf("Error unmarshalling the Cloudability response body: %v\n", err)
	}

	if responseData.Pagination.Next != "" {
		log.Fatal("Cloudability result is unexpectedly paginated")
	}

	return responseData
}

func getApptioOpentoken(configMap Configuration, client http.Client) string {
	apiKeyPairAny := getMapKeyValue(configMap, "api_key_pair", "cloudability")
	apiKeyPair, ok := apiKeyPairAny.([]any)
	if !ok {
		log.Fatalf("Error reading Cloudability API keypair, expected an array, found %v",
			reflect.TypeOf(apiKeyPairAny).String())
	}
	if len(apiKeyPair) != 2 {
		log.Fatalf("Error reading Cloudability API keypair, expected 2 items, found %d",
			len(apiKeyPair))
	}
	apiAccessKey, ok1 := apiKeyPair[0].(string)
	apiSecret, ok2 := apiKeyPair[1].(string)
	if !ok1 || !ok2 {
		log.Fatalf(
			"Error reading Cloudability API keypair, expected entries to be strings, found %v and %v",
			reflect.TypeOf(apiKeyPair[0]).String(), reflect.TypeOf(apiKeyPair[1]).String())
	}
	body := bytes.NewBufferString(`{"keyAccess":"` + apiAccessKey + `","keySecret":"` + apiSecret + `"}`)
	authRequest, err := http.NewRequest("POST", "https://frontdoor.apptio.com/service/apikeylogin", body)
	if err != nil {
		log.Fatalf("Error creating Cloudability authorization request:  %v", err)
	}
	authRequest.Header.Add("Accept", "application/json")
	authRequest.Header.Add("content-type", "application/json")

	log.Println("[getCloudabilityData] Sending request for authorization")
	authResponse, err := client.Do(authRequest)
	if err != nil {
		log.Fatalf("Error sending authorization request to Cloudability:  %v", err)
	}
	if authResponse.StatusCode != http.StatusOK {
		log.Fatalf("Error getting authorization data from Cloudability:  %d, %q",
			authResponse.StatusCode, authResponse.Status)
	}
	defer func(Body io.ReadCloser) {
		if err := Body.Close(); err != nil {
			log.Fatalf("Ignoring error closing Cloudability body: %v", err)
		}
	}(authResponse.Body)
	return authResponse.Header.Get("apptio-opentoken")
}

func getSheetDataFromCloudability(
	cldy *CloudabilityCostData,
	accountsMetadata map[string]*AccountMetadata,
	configMap Configuration,
	costCells map[string]map[string]float64,
	columnHeadsSet map[string]struct{},
	metadata map[string]providerAccountMetadata,
) {
	// Build a two-dimensional map in which the first key is the account ID,
	// the second key is the usage family, and the value is the corresponding
	// cost -- this amounts to a sparse sheet grid.  While we're at it, collect
	// the column headers for the grid (using a map "trick" where we only care
	// about the keys), and collect some metadata for each account.
	ignored := make(map[string]struct{}) // Suppress multiple warnings
	for _, entry := range cldy.Results {
		// Skip accounts that we're not looking for, but keep a list of them so
		// that we don't issue multiple warnings for them; warn about accounts
		// attributed to our cost center that we're not currently tracking.
		if skipAccountEntry(
			accountsMetadata[entry.AccountID],
			entry.AccountID,
			entry.CostCenter,
			entry.CloudProvider,
			entry.AccountName,
			ignored,
			configMap,
			"Cloudability",
		) {
			continue
		}

		// Note the current entry's usage family so that we can use it as a
		// column header; and, if this is the first time we've seen this
		// account, note its account-specific metadata.
		columnHeadsSet[entry.UsageFamily] = struct{}{}
		if _, exists := metadata[entry.AccountID]; !exists {
			metadata[entry.AccountID] = providerAccountMetadata{
				AccountName:    entry.AccountName,
				CloudProvider:  entry.CloudProvider,
				CostCenter:     entry.CostCenter,
				Date:           cldy.Meta.Dates.Start.Format("2006-01"),
				PayerAccountId: entry.PayerAccountId,
			}
		}

		// Capture the cost data.  If this is the first data for this account,
		// create its "row".  If the cell has already been written, exit with
		// an error.
		cost, err := strconv.ParseFloat(entry.Cost, 64)
		if err != nil {
			log.Fatalf("Error parsing %s:%s Cost value (%v) as a float: %v",
				entry.AccountID, entry.UsageFamily, entry.Cost, err)
		}
		if _, exists := costCells[entry.AccountID]; !exists {
			costCells[entry.AccountID] = make(map[string]float64)
		}
		if _, exists := costCells[entry.AccountID][entry.UsageFamily]; exists {
			log.Fatalf(
				"Duplicate entry for %s:%s, values %f and %f",
				entry.AccountID,
				entry.UsageFamily,
				costCells[entry.AccountID][entry.UsageFamily],
				cost)
		}
		costCells[entry.AccountID][entry.UsageFamily] = cost
	}
}
