package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/api/sheets/v4"
	"gopkg.in/yaml.v2"
)

type CommandLineOptions struct {
	debugPtr          *bool
	awsWriteTagsPtr   *bool
	accountsFilePtr   *string
	taggedAccountsPtr *bool
	monthPtr          *string
	costTypePtr       *string
	csvfilePtr        *string
	reportFilePtr     *string
	outputTypePtr     *string
}

type AccountsFile struct {
	Configuration map[string]Configuration `yaml:"configuration"`
	Providers     map[string]Team          `yaml:"cloud_providers"`
}

type Configuration map[string]any
type Team map[string][]AccountEntry

// AccountEntry describes an account with metadata.
// If Pattern is set (instead of AccountID), any account whose ID starts with
// the pattern string is automatically matched and assigned to this entry's
// group and provider.
type AccountEntry struct {
	AccountID        string  `yaml:"accountid"`
	Pattern          string  `yaml:"pattern"`
	StandardValue    float64 `yaml:"standardvalue"`
	DeviationPercent int     `yaml:"deviationpercent"`
	Category         string  `yaml:"category"`
	Description      string  `yaml:"description"`
}

func main() {
	log.Println("[main] costpuller starting.")
	nowTime := time.Now()
	lastMonth := time.Date(nowTime.Year(), nowTime.Month()-1, 1, 0, 0, 0, 0, nowTime.Location())
	nowStr := nowTime.Format("20060102150405")
	defaultMonth := lastMonth.Format("2006-01")
	defaultCsvFile := fmt.Sprintf("output-%s.csv", defaultMonth)
	defaultReportFile := fmt.Sprintf("report-%s.txt", nowStr)
	options := CommandLineOptions{
		accountsFilePtr:   flag.String("accounts", "accounts.yaml", "file to read accounts list from"),
		awsWriteTagsPtr:   flag.Bool("awswritetags", false, "write tags to AWS accounts (USE WITH CARE!)"),
		costTypePtr:       flag.String("costtype", "AmortizedCost", `cost type to pull, one of "AmortizedCost", "BlendedCost", "NetAmortizedCost", "NetUnblendedCost", "NormalizedUsageAmount", "UnblendedCost", or "UsageQuantity"`),
		csvfilePtr:        flag.String("csv", defaultCsvFile, "output file for csv data"),
		debugPtr:          flag.Bool("debug", false, "outputs debug info"),
		monthPtr:          flag.String("month", defaultMonth, `context month in format yyyy-mm`),
		outputTypePtr:     flag.String("output", "gsheet", `output destination, needs to be one of "csv" or "gsheet"`),
		reportFilePtr:     flag.String("report", defaultReportFile, "output file for data consistency report"),
		taggedAccountsPtr: flag.Bool("taggedaccounts", false, "use the AWS tags as account list source"),
	}
	flag.Parse()

	if *options.csvfilePtr == defaultCsvFile && *options.monthPtr != defaultMonth {
		newDefaultCsvFile := fmt.Sprintf("output-%s.csv", *options.monthPtr)
		options.csvfilePtr = &newDefaultCsvFile
	}
	accountsFile, err := loadAccountsFile(*options.accountsFilePtr)
	if err != nil {
		log.Fatalf("[main] error loading accounts file: %v", err)
	}
	if len(accountsFile.Configuration) == 0 {
		log.Fatalf("[main] error in accounts file: empty or missing \"configuration\" section")
	}
	if len(accountsFile.Providers) == 0 {
		log.Fatalf("[main] error in accounts file: empty or missing \"cloud_providers\" section")
	}
	accountMetadata, accountPatterns := getAccountMetadata(accountsFile.Providers)

	output := newOutputObject(options, accountsFile)
	defer output.close()

	var sheetData []*sheets.RowData

	cldy, useCldyData := accountsFile.Configuration["cloudability"]
	if *options.awsWriteTagsPtr || !useCldyData {
		awsConfig := getMapKeyValue(accountsFile.Configuration, "aws", "configuration")
		awsProfile := getMapKeyString(awsConfig, "profile", "")
		if awsProfile == "" {
			awsProfile = "default"
			log.Printf(
				"[main] no \"profile\" key found in the \"aws\" section of the configuration file; "+
					"using AWS credentials profile %q",
				awsProfile,
			)
		}
		awsPuller := NewAwsPuller(awsProfile, *options.debugPtr)

		if *options.awsWriteTagsPtr {
			writeAwsTags(awsPuller, options)
			os.Exit(0)
		}

		reportFile := getReportFile(options)
		defer closeFile(reportFile)

		awsAccounts, sortedAccountKeys := awsPuller.getAwsAccounts(accountsFile, options)

		sheetData = awsPuller.pullAwsByAccount(awsAccounts, sortedAccountKeys, options, reportFile)
	} else {
		costCells := make(map[string]map[string]float64)
		columnHeadsSet := make(map[string]struct{}) // This is the Go equivalent of a "set".
		metadata := make(map[string]providerAccountMetadata)

		cldyCostData := getCloudabilityData(cldy, options)
		if cldyCostData == nil || cldyCostData.TotalResults == 0 || len(cldyCostData.Results) == 0 {
			log.Fatalf("[main] no Cloudability data")
		}
		getSheetDataFromCloudability(cldyCostData, accountMetadata, accountPatterns, cldy, costCells, columnHeadsSet, metadata)

		ibmc, fetchIbmcloudData := accountsFile.Configuration["ibmcloud"]
		if fetchIbmcloudData {
			ibmCostData := getIbmcloudData(ibmc, options)
			if ibmCostData == nil || len(ibmCostData) == 0 {
				log.Fatal("[main] no IBM Cloud data")
			}
			getSheetDataFromIbmcloud(ibmCostData, accountMetadata, accountPatterns, ibmc, costCells, metadata)
		}

		checkMissing(accountMetadata, cldyCostData)

		sheetData = getSheetFromCostCells(costCells, columnHeadsSet, accountMetadata, metadata)
	}

	output.writeSheet(sheetData)

	log.Println("[main] operation done")
}

// OutputObject encapsulates the destination for the output, hiding the details
// of whether it goes to a local CSV file or a Google sheet (or both).
type OutputObject struct {
	csvFile      *os.File
	httpClient   *http.Client
	gsheetConfig Configuration
	refTime      time.Time
}

func newOutputObject(options CommandLineOptions, accountsFile AccountsFile) *OutputObject {
	refTime, err := time.Parse("2006-01", *options.monthPtr)
	if err != nil {
		log.Fatalf("[main] error parsing month value, %q: %v", *options.monthPtr, err)
	}

	obj := &OutputObject{refTime: refTime}

	if *options.outputTypePtr == "csv" {
		obj.csvFile = getCsvFile(options)
	} else if *options.outputTypePtr == "gsheet" {
		oauthConfig := getMapKeyValue(accountsFile.Configuration, "oauth", "configuration")
		obj.httpClient = getGoogleOAuthHttpClient(oauthConfig)
		obj.gsheetConfig = getMapKeyValue(accountsFile.Configuration, "gsheet", "configuration")
	} else {
		log.Fatalf("[main] Unexpected value for output type, %q", *options.outputTypePtr)
	}
	return obj
}

func (o *OutputObject) writeSheet(sheetData []*sheets.RowData) {
	if sheetData == nil || len(sheetData) == 0 {
		log.Fatal("[writeSheet] no sheet data")
	}
	if o.csvFile != nil {
		err := writeCsvFromSheet(o.csvFile, sheetData)
		if err != nil {
			log.Fatalf("[writeSheet] error writing to output file: %v", err)
		}
	}
	if o.httpClient != nil {
		postToGSheet(sheetData, o.httpClient, o.gsheetConfig, o.refTime)
	}
}

func (o *OutputObject) close() {
	if o.csvFile != nil {
		err := o.csvFile.Close()
		if err != nil {
			log.Printf("Ignoring error closing csv file: %v", err)
		}
	}
	if o.httpClient != nil {
		o.httpClient.CloseIdleConnections()
	}
}

func (a *AwsPuller) getAwsAccounts(
	accountsFile AccountsFile,
	options CommandLineOptions,
) (accounts map[string][]AccountEntry, keys []string) {
	//var accounts map[string][]AccountEntry
	if *options.taggedAccountsPtr {
		a, err := getAccountSetsFromAws(a)
		if err != nil {
			log.Fatalf("[getAwsAccounts] error getting accounts list: %v", err)
		}
		accounts = a
	} else {
		accounts = getMapKeyValue(accountsFile.Providers, "aws", "cloud_providers")
	}
	if len(accounts) == 0 {
		fmt.Println("[getAwsAccounts] Warning:  No AWS accounts found!")
	}
	return accounts, sortedKeys(accounts)
}

func (a *AwsPuller) pullAwsByAccount(
	accounts map[string][]AccountEntry,
	sortedAccountKeys []string,
	options CommandLineOptions,
	reportFile *os.File,
) (sheetData []*sheets.RowData) {
	if *options.monthPtr == "" || *options.costTypePtr == "" {
		log.Fatal("[pullAwsByAccount] missing month or cost type (use --month=yyyy-mm, --costtype=type)")
	}
	for _, group := range sortedAccountKeys {
		accountList := accounts[group]
		if len(accountList) == 0 {
			log.Printf("[pullAwsByAccount] Warning: no accounts found in group %q!", group)
		}
		for _, account := range accountList {
			log.Printf("[pullAwsByAccount] pulling data for account %s (group %s)\n", account.AccountID, group)
			rowData, _, err := a.pullAwsAccount(
				account,
				group,
				*options.monthPtr,
				*options.costTypePtr,
				reportFile,
			)
			if err != nil {
				log.Fatalf("[pullAwsByAccount] error pulling data: %v", err)
			}
			sheetData = append(sheetData, rowData)
		}
	}
	return
}

func writeAwsTags(awsPuller *AwsPuller, options CommandLineOptions) {
	accountsFile, err := loadAccountsFile(*options.accountsFilePtr)
	if err != nil {
		log.Fatalf("[writeAwsTags] error getting accounts list: %v", err)
	}
	accounts := getMapKeyValue(accountsFile.Providers, "aws", "cloud_providers")
	err = awsPuller.WriteAwsTags(accounts)
	if err != nil {
		log.Fatalf("[writeAwsTags] error writing account tag: %v", err)
	}
}

func getCsvFile(options CommandLineOptions) *os.File {
	outfile, err := os.Create(*options.csvfilePtr)
	if err != nil {
		log.Fatalf("[getCsvFile] error creating output file: %v", err)
	}
	log.Printf("[getCsvFile] using csv output file %s\n", *options.csvfilePtr)
	return outfile
}

func getReportFile(options CommandLineOptions) *os.File {
	reportFile, err := os.Create(*options.reportFilePtr)
	if err != nil {
		log.Fatalf("[getReportFile] error creating report file: %v", err)
	}
	log.Printf("[getReportFile] using report output file %s\n", *options.reportFilePtr)
	return reportFile
}

func sortedKeys[T any](m map[string]T) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (a *AwsPuller) pullAwsAccount(
	account AccountEntry,
	group string,
	month string,
	costType string,
	reportFile *os.File,
) (normalized *sheets.RowData, total float64, err error) {
	result, err := a.PullData(account.AccountID, month, costType)
	if err != nil {
		log.Fatalf("[pullAwsAccount] error pulling data from AWS for account %s: %v", account.AccountID, err)
	}
	total, err = a.CheckResponseConsistency(account, result)
	if err != nil {
		log.Printf(
			"[pullAwsAccount] consistency check failed on response for account data %s: %v",
			account.AccountID,
			err,
		)
		writeReport(reportFile, account.AccountID+": "+err.Error())
	}
	normalized, err = a.NormalizeResponse(group, month, account.AccountID, result)
	if err != nil {
		log.Fatalf("[pullAwsAccount] error normalizing data from AWS for account %s: %v", account.AccountID, err)
	}
	return
}

func writeCsvFromSheet(outfile *os.File, data []*sheets.RowData) error {
	writer := csv.NewWriter(outfile)
	defer writer.Flush()
	for _, row := range data {
		rowData := make([]string, len(row.Values))
		for i, cell := range row.Values {
			var cellData string
			if cell.UserEnteredValue.StringValue != nil {
				cellData = *cell.UserEnteredValue.StringValue
			} else if cell.UserEnteredValue.FormulaValue != nil {
				cellData = *cell.UserEnteredValue.FormulaValue
			} else if cell.UserEnteredValue.NumberValue != nil {
				cellData = fmt.Sprintf("%f", *cell.UserEnteredValue.NumberValue)
			} else {
				log.Fatalf("Unexpected sheet cell value:  %v", cell.UserEnteredValue)
			}
			rowData[i] = cellData
		}
		err := writer.Write(rowData)
		if err != nil {
			log.Printf("[writeCsvFromSheet] error writing csv data to file: %v ", err)
			return err
		}
	}
	return nil
}

func writeReport(outfile *os.File, data string) {
	_, err := outfile.WriteString(data + "\n")
	if err != nil {
		log.Printf("[writeReport] error writing report data to file: %v ", err)
	}
}

func loadAccountsFile(accountsFileName string) (accountsFile AccountsFile, err error) {
	yamlFile, err := os.ReadFile(accountsFileName)
	if err != nil {
		return accountsFile, fmt.Errorf("[loadAccountsFile] error loading accounts file: %v", err)
	}
	accountsFile = AccountsFile{
		Configuration: make(map[string]Configuration),
		Providers:     make(map[string]Team),
	}
	err = yaml.Unmarshal(yamlFile, accountsFile)
	if err != nil {
		return accountsFile, fmt.Errorf("[loadAccountsFile] error unmarshalling accounts file: %v", err)
	}
	// set category manually on all entries
	for _, group := range accountsFile.Providers {
		for category, accountEntries := range group {
			for _, accountEntry := range accountEntries {
				accountEntry.Category = category
			}
		}
	}
	return
}

func getAccountSetsFromAws(awsPuller *AwsPuller) (map[string][]AccountEntry, error) {
	log.Println("[getAccountSetsFromAws] initiating account metadata pull")
	metadata, err := awsPuller.GetAwsAccountMetadata()
	if err != nil {
		log.Fatalf("[getAccountSetsFromAws] error getting accounts list from metadata: %v", err)
	}
	log.Println("[getAccountSetsFromAws] processing account metadata pull")
	accounts := make(map[string][]AccountEntry)
	for accountID, accountMetadata := range metadata {
		if category, ok := accountMetadata[AwsTagCostpullerCategory]; ok {
			description := accountMetadata[AwsMetadataDescription]
			log.Printf("tagged category (\"%s\") found for account %s (\"%s\")", category, accountID, description)
			status := accountMetadata[AwsMetadataStatus]
			if status == "ACTIVE" {
				if _, ok := accounts[category]; !ok {
					accounts[category] = []AccountEntry{}
				}
				accounts[category] = append(accounts[category], AccountEntry{
					AccountID:        accountID,
					StandardValue:    0,
					DeviationPercent: 0,
					Category:         category,
					Description:      description,
				})
			}
		} else {
			// account without category tag
			log.Printf(
				"ERROR: account %s does not have an aws tag set for category (\"%s\")",
				accountID,
				accountMetadata[AwsMetadataDescription],
			)
		}
	}
	return accounts, nil
}

// AccountMetadata is an object which encapsulates the information from the
// accounts YAML file which is associated with a given account.
type AccountMetadata struct {
	AccountId     string
	Category      string
	CloudProvider string
	DataFound     bool
	Description   string
	Group         string
}

// PatternEntry holds a prefix pattern from the accounts YAML file along with
// the provider and group it belongs to, so that unknown accounts can be
// matched dynamically.
type PatternEntry struct {
	Prefix        string
	CloudProvider string
	Group         string
	Category      string
	Description   string
}

var accountIdPatterns = map[string]*regexp.Regexp{
	"Amazon": regexp.MustCompile(`^([0-9]{4})-?([0-9]{4})-?([0-9]{4})$`),                                         // e.g., "5901-8385-7305"
	"Azure":  regexp.MustCompile(`^([0-9a-f]{8})-?([0-9a-f]{4})-?([0-9a-f]{4})-?([0-9a-f]{4})-?([0-9a-f]{12})$`), // e.g., "b0ad4737-8299-4c0a-9dd5-959cbcf8d81c"
}

// getAccountMetadata takes the hierarchy from the accounts YAML file and
// inverts it, so that, given an account ID, we can find the cloud provider
// and group that the account is associated with.  Entries with a Pattern
// field (instead of AccountID) are collected separately and returned as
// patterns for dynamic matching.
func getAccountMetadata(providers map[string]Team) (metadata map[string]*AccountMetadata, patterns []PatternEntry) {
	metadata = make(map[string]*AccountMetadata)
	for provider, groups := range providers {
		if provider == "aws" { // Convert for historical compatibility
			provider = "Amazon"
		}
		for group, groupEntries := range groups {
			for _, entry := range groupEntries {
				if entry.Pattern != "" {
					patterns = append(patterns, PatternEntry{
						Prefix:        entry.Pattern,
						CloudProvider: provider,
						Group:         group,
						Category:      entry.Category,
						Description:   entry.Description,
					})
					continue
				}
				// Use the account ID as the key to the map.  Amazon and Azure
				// use IDs with a fixed format -- check that the ID from the
				// accounts file matches the format.  For historical
				// compatibility, we accept IDs which contain no hyphens, but
				// we add the hyphens to match the format that Cloudability uses.
				var key string
				translate, exists := accountIdPatterns[provider]
				if exists {
					if matches := translate.FindStringSubmatch(entry.AccountID); matches != nil {
						key = strings.Join(matches[1:], "-")
					} else {
						log.Fatalf("[getAccountMetadata] unrecognized account id format, %q, must match %q",
							entry.AccountID, translate.String())
					}
				} else {
					key = entry.AccountID
				}
				metadata[key] = &AccountMetadata{
					AccountId:     entry.AccountID,
					Category:      entry.Category,
					CloudProvider: provider,
					DataFound:     false, // Will be set when cost data is found
					Description:   entry.Description,
					Group:         group,
				}
			}
		}
	}

	return
}

// closeFile is a helper function which allows closing a file to be deferred
// and which ignores any errors.
func closeFile(filename *os.File) {
	_ = filename.Close()
}

// getMapKeyValue is a generic helper function which fetches a value from the
// given key in the given map; if the key is not in the map, and the caller has
// provided the section name, the program exits with an error; otherwise, it
// returns the "zero value".
func getMapKeyValue[V any](configMap map[string]V, key string, section string) (value V) {
	if value, ok := configMap[key]; ok {
		return value
	}

	if section != "" {
		log.Fatalf("Key %q is missing from the %q section of the configuration file", key, section)
	}

	return
}

// getMapKeyValue is a generic helper function which fetches a string from the
// given key in the given map; if the key is not in the map or the value is not
// a string, and the caller has provided the section name, the program exits
// with an error; otherwise, it returns the "zero value".
func getMapKeyString(configMap map[string]any, key string, section string) (value string) {
	valueAny := getMapKeyValue(configMap, key, section)
	if value, ok := valueAny.(string); ok {
		return value
	}

	if valueAny != nil {
		msg := "%q key in the "
		if section != "" {
			msg += fmt.Sprintf("%q section of the ", section)
		}
		log.Fatalf(msg+"configuration file must be a string; found %v, type %T",
			key, valueAny, valueAny)
	}

	return
}

// getStringFromAny encapsulates and centralizes the operation of converting an
// `any` value to a string and takes care of checking for and handling failures.
func getStringFromAny(anyValue any, message string) (value string) {
	value, ok := anyValue.(string)
	if !ok && anyValue != nil {
		log.Fatalf("Unexpected value (%v) for %s, expected a string", anyValue, message)
	}
	return
}

// skipAccountEntry is a helper function which determines whether to skip
// account entries that we're not looking for.  It updates a list of them so
// that we don't issue multiple warnings for them; it warns about account
// entries attributed to our cost center that we're not currently tracking.
// If the account is not explicitly listed but matches a prefix pattern, it
// is dynamically added to the metadata map and processed normally.
func skipAccountEntry(
	accountsMetadata map[string]*AccountMetadata,
	patterns []PatternEntry,
	accountId string,
	costCenter string,
	providerConfigName string,
	accountName string,
	ignored map[string]struct{},
	configMap Configuration,
	dataSource string,
) bool {
	accountMetadata := accountsMetadata[accountId]
	if accountMetadata == nil {
		ourCostCenter := getMapKeyString(configMap, "cost_center", "")
		for _, p := range patterns {
			if strings.HasPrefix(accountId, p.Prefix) && costCenter == ourCostCenter {
				accountsMetadata[accountId] = &AccountMetadata{
					AccountId:     accountId,
					Category:      p.Category,
					CloudProvider: p.CloudProvider,
					DataFound:     false,
					Description:   p.Description,
					Group:         p.Group,
				}
				accountMetadata = accountsMetadata[accountId]
				break
			}
		}
	}
	if accountMetadata == nil {
		if _, exists := ignored[accountId]; !exists {
			ourCostCenter := getMapKeyString(configMap, "cost_center", "")
			if costCenter == ourCostCenter {
				log.Printf("Warning:  found account which is not in the accounts file:  "+
					"%s:%s:%s:%s (%s); ignoring",
					dataSource, costCenter, providerConfigName, accountId, accountName)
			}
			ignored[accountId] = struct{}{}
		}
		return true
	}
	// Note the cloud provider which corresponds to the account ID, and
	// warn about errors in the accounts file.
	if accountMetadata.CloudProvider != providerConfigName &&
		// Accept "AWS" as an alias for "Amazon"
		!(providerConfigName == "Amazon" && accountMetadata.CloudProvider == "AWS") {
		log.Printf(
			"For account %s, the accounts file has cloud provider %q, but it should be %q; using %q",
			accountId,
			accountMetadata.CloudProvider,
			providerConfigName,
			providerConfigName,
		)
		accountMetadata.CloudProvider = providerConfigName
	}
	// Mark this account as "found" so that we can report ones which aren't.
	accountMetadata.DataFound = true
	return false
}

func checkMissing(accountsMetadata map[string]*AccountMetadata, cldy *CloudabilityCostData) {
	// Check for accounts from the YAML file which were not found in the
	// Cloudability data.
	var filters []string
	for id, entry := range accountsMetadata {
		if !entry.DataFound {
			if filters == nil {
				for _, filter := range cldy.Meta.Filters {
					filters = append(filters, fmt.Sprintf("%q %s %q", filter.Label, filter.Comparator, filter.Value))
				}
			}
			msg := fmt.Sprintf("Warning:  no data source found for account %s:%s:%s",
				entry.CloudProvider, entry.Group, id)
			msg += fmt.Sprintf("; filters: %s", strings.Join(filters, " && "))
			log.Printf(msg)
		}
	}
}
