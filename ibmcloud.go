package main

import (
	"github.com/IBM/platform-services-go-sdk/usagereportsv4"
	"log"
	"strconv"

	"github.com/IBM/go-sdk-core/v5/core"
	"github.com/IBM/platform-services-go-sdk/enterpriseusagereportsv1"
)

const ConfigSect = "ibmcloud" // Key in the 'configuration' section of the accounts YAML file
const CloudProvider = "IBM"   // Key in the 'cloud_providers' section of the accounts YAML file

type IbmcResultsEntry struct {
	ResultsEntry
	Data *usagereportsv4.AccountSummary
}

func getIbmcloudData(configMap Configuration, options CommandLineOptions) []IbmcResultsEntry {
	accountIdStr := getMapKeyString(configMap, "account_id", ConfigSect)

	log.Println("[getIbmcloudData] creating session")
	authenticator, err := core.NewIamAuthenticatorBuilder().
		SetApiKey(getMapKeyString(configMap, "api_key", ConfigSect)).
		Build()
	if err != nil {
		log.Fatalf("Error creating IBM Cloud authenticator: %v", err)
	}

	eurOpts := enterpriseusagereportsv1.EnterpriseUsageReportsV1Options{
		//URL:           getMapKeyString(configMap, "endpoint", ConfigSect),  // The default works.
		Authenticator: authenticator,
	}
	eurServiceClient, err := enterpriseusagereportsv1.NewEnterpriseUsageReportsV1(&eurOpts)
	if err != nil {
		log.Fatalf("Error creating IBM Cloud enterprise usage reports client: %v", err)
	}

	grurOpts := eurServiceClient.NewGetResourceUsageReportOptions().
		SetAccountGroupID(accountIdStr).
		SetMonth(*options.monthPtr)

	costCenter := getAccountGroupName(grurOpts, eurServiceClient)
	result := getUsageReport(grurOpts, eurServiceClient)

	urOpts := usagereportsv4.UsageReportsV4Options{Authenticator: authenticator} // Use the default URL
	urServiceClient, err := usagereportsv4.NewUsageReportsV4(&urOpts)
	if err != nil {
		log.Fatalf("Error creating IBM Cloud Usage Reports client: %v", err)
	}

	return getAccountResults(result, costCenter, *options.monthPtr, urServiceClient)
}

func getAccountResults(
	result *enterpriseusagereportsv1.Reports,
	costCenter string,
	month string,
	urServiceClient *usagereportsv4.UsageReportsV4,
) (returnValue []IbmcResultsEntry) {
	for _, account := range result.Reports {
		resultEntry := IbmcResultsEntry{
			ResultsEntry: ResultsEntry{
				AccountID:      *account.EntityID,
				AccountName:    *account.EntityName,
				CloudProvider:  CloudProvider,
				Cost:           strconv.FormatFloat(*account.BillableCost, 'f', 2, 64),
				CostCenter:     costCenter,
				PayerAccountId: *account.BillingUnitID,
			},
		}

		log.Printf("[getIbmcloudData] getting account summary for %s", *account.EntityID)
		summaryOpts := urServiceClient.NewGetAccountSummaryOptions(*account.EntityID, month)
		as, response, err := urServiceClient.GetAccountSummary(summaryOpts)
		if err != nil {
			log.Fatalf("Error getting IBM Cloud account summary: %v", err)
		}
		if response.StatusCode != 200 {
			log.Fatalf(
				"HTTP error %d getting IBM Cloud account summary: %v",
				response.StatusCode,
				response,
			)
		}
		resultEntry.Data = as
		returnValue = append(returnValue, resultEntry)
	}
	return
}

func getAccountGroupName(
	serviceOpts *enterpriseusagereportsv1.GetResourceUsageReportOptions,
	serviceClient *enterpriseusagereportsv1.EnterpriseUsageReportsV1,
) string {
	serviceOpts.SetChildren(false) // Get the account group itself
	result := serviceCall(serviceOpts, serviceClient, "account group")
	return *result.Reports[0].EntityName
}

func getUsageReport(
	serviceOptions *enterpriseusagereportsv1.GetResourceUsageReportOptions,
	serviceClient *enterpriseusagereportsv1.EnterpriseUsageReportsV1,
) *enterpriseusagereportsv1.Reports {
	serviceOptions.SetChildren(true) // Get the accounts in the group
	return serviceCall(serviceOptions, serviceClient, "enterprise summaries")
}

func serviceCall(
	serviceOptions *enterpriseusagereportsv1.GetResourceUsageReportOptions,
	serviceClient *enterpriseusagereportsv1.EnterpriseUsageReportsV1,
	logId string,
) *enterpriseusagereportsv1.Reports {
	log.Printf("[getIbmcloudData] getting %s", logId)
	result, response, err := serviceClient.GetResourceUsageReport(serviceOptions)
	if err != nil {
		log.Fatalf("Error getting IBM Cloud %s: %v", logId, err)
	}
	if response.StatusCode != 200 {
		log.Fatalf("HTTP error %d getting IBM Cloud %s: %v",
			response.StatusCode, logId, response)
	}
	return result
}

// getSheetDataFromIbmcloud converts the cost data into a Google Sheet.
func getSheetDataFromIbmcloud(
	accounts []IbmcResultsEntry,
	accountsMetadata map[string]*AccountMetadata,
	patterns []PatternEntry,
	configMap Configuration,
	costCells map[string]map[string]float64,
	metadata map[string]providerAccountMetadata,
) {
	// Build a two-dimensional map in which the first key is the account ID,
	// the second key is the usage family, and the value is the corresponding
	// cost -- this amounts to a sparse sheet grid.  While we're at it, collect
	// the column headers for the grid (using a map "trick" where we only care
	// about the keys), and collect some metadata for each account.
	ignored := make(map[string]struct{}) // Suppress multiple warnings
	for _, accountSummary := range accounts {
		// Skip accounts that we're not looking for, but keep a list of them so
		// that we don't issue multiple warnings for them; warn about accounts
		// attributed to our cost center that we're not currently tracking.
		accountId := accountSummary.AccountID
		if skipAccountEntry(
			accountsMetadata,
			patterns,
			accountId,
			accountSummary.CostCenter,
			accountSummary.CloudProvider,
			accountSummary.AccountName,
			ignored,
			configMap,
			"IBM Cloud",
		) {
			continue
		}

		// Create the "row" for this account's costs.
		if _, exists := costCells[accountId]; !exists {
			costCells[accountId] = make(map[string]float64)
		} else {
			log.Fatalf(
				"[getSheetDataFromIbmcloud] Cost cell row for account %q already exists",
				accountId)
		}
		// Note this account's account-specific metadata.
		metadata[accountId] = providerAccountMetadata{
			AccountName:    accountSummary.AccountName,
			CloudProvider:  accountSummary.CloudProvider,
			CostCenter:     accountSummary.CostCenter,
			Date:           *accountSummary.Data.Month,
			PayerAccountId: accountSummary.PayerAccountId,
		}

		for _, resource := range accountSummary.Data.AccountResources {
			// Place costs according to their resource ID into the Cloudability
			// "Usage Family" buckets.
			//
			// Note:  in several cases, the bucketing is arbitrary and probably
			//        incorrect....
			bucket := "Other"
			switch *resource.ResourceName {
			case "Block Storage for VPC",
				"Cloud Object Storage":
				bucket = "Storage"
			case "Cloud Activity Tracker", "Cloud Monitoring":
				bucket = "Notifications"
			case "Continuous Delivery", "Log Analysis":
				bucket = "Other"
			case "Floating IP for VPC":
				bucket = "IP Address"
			case "Kubernetes Service":
				bucket = "Instance Usage"
			case "Load Balancer for VPC":
				bucket = "Load Balancer"
			case "Virtual Private Cloud":
				bucket = "VPN"
			case "Virtual Private Endpoint for VPC", "Virtual Server for VPC":
				bucket = "VPC Endpoint"
			default:
				log.Printf(
					"[getSheetDataFromIbmcloud] unexpected resource %q (%s); using category %q",
					*resource.ResourceName, *resource.ResourceID, bucket)
			}

			costCells[accountId][bucket] += *resource.BillableCost

			//for _, plan := range resource.Plans {
			//	for _, usage := range plan.Usage {
			//		bucket := "Other"
			//		switch *usage.Metric {
			//
			//		// This was attempt #2
			//
			//		case "VCPU_HOURS", "MEMORY_HOURS", "DISK_HOURS", "RHEL_INSTANCE_HOURS", "OCP_VCPU_HOURS":
			//			bucket = "Instance Usage"
			//		case "GIGABYTE_HOURS":
			//			bucket = "Storage"
			//		case "INSTANCE_HOURS", "GIGABYTE_MONTHS":
			//			bucket = "Load Balancer"
			//		case "INSTANCES":
			//			bucket = "IP Address" // "VPC Endpoint"??
			//		case "GIGABYTE_TRANSMITTED_OUTBOUNDS":
			//			bucket = "VPN" // "VPC Endpoint" ??
			//		case "STANDARD_CLASS_A_CALLS", "STANDARD_CLASS_B_CALLS",
			//			"SMART_TIER_CLASS_A_CALLS", "SMART_TIER_CLASS_B_CALLS",
			//			"STANDARD_BANDWIDTH", "SMART_TIER_BANDWIDTH",
			//			"SMART_TIER_RETRIEVAL":
			//			bucket = "Storage Access"
			//		case "SMART_TIER_HOT_STORAGE", "SMART_TIER_COOL_STORAGE",
			//			"SMART_TIER_COLD_STORAGE", "STANDARD_STORAGE":
			//			bucket = "Storage"
			//			//case "GIGABYTE_MONTHS":  // Duplicate metric with "Load Balancer" case
			//			//	bucket = "Notifications"
			//		case "GIGABYTE_TRANSMITTED", "INSTANCE_HOUR":
			//			bucket = "VPC Endpoint"
			//		case "AUTHORIZED_USERS_PER_MONTH", "JOB_EXECUTIONS_PER_MONTH":
			//			bucket = "Other" // "Continuous Delivery / Lite"
			//
			//		// This was attempt #1
			//
			//		case "AUTHORIZED_USERS_PER_MONTH":
			//			bucket = "Other"
			//		case "DISK_HOURS":
			//			bucket = "Storage"
			//		case "GIGABYTE_HOURS":
			//			bucket = "Storage"
			//		case "GIGABYTE_MONTHS":
			//			bucket = "Storage"
			//		case "GIGABYTE_TRANSMITTED":
			//			bucket = "Data Transfer"
			//		case "GIGABYTE_TRANSMITTED_OUTBOUNDS":
			//			bucket = "Data Transfer"
			//		case "INSTANCE_HOUR":
			//			bucket = "Instance Usage"
			//		case "INSTANCE_HOURS":
			//			bucket = "Instance Usage"
			//		case "INSTANCES":
			//			bucket = "Instance Usage"
			//		case "JOB_EXECUTIONS_PER_MONTH":
			//			bucket = "Instance Usage"
			//		case "MEMORY_HOURS":
			//			bucket = "Instance Usage"
			//		case "OCP_VCPU_HOURS":
			//			bucket = "Instance Usage"
			//		case "RHEL_INSTANCE_HOURS":
			//			bucket = "Instance Usage"
			//		case "SMART_TIER_BANDWIDTH":
			//			bucket = "Data Transfer"
			//		case "SMART_TIER_CLASS_A_CALLS":
			//			bucket = "API Request"
			//		case "SMART_TIER_CLASS_B_CALLS":
			//			bucket = "API Request"
			//		case "SMART_TIER_COLD_STORAGE":
			//			bucket = "Storage"
			//		case "SMART_TIER_COOL_STORAGE":
			//			bucket = "Storage"
			//		case "SMART_TIER_HOT_STORAGE":
			//			bucket = "Storage"
			//		case "SMART_TIER_RETRIEVAL":
			//			bucket = "Data Transfer"
			//		case "STANDARD_BANDWIDTH":
			//			bucket = "Data Transfer"
			//		case "STANDARD_CLASS_A_CALLS":
			//			bucket = "API Request"
			//		case "STANDARD_CLASS_B_CALLS":
			//			bucket = "API Request"
			//		case "STANDARD_STORAGE":
			//			bucket = "Storage"
			//		case "VCPU_HOURS":
			//			bucket = "Instance Usage"
			//		default:
			//			log.Printf(
			//				"[getSheetDataFromIbmcloud] unexpected resource %q; using category %q",
			//				*usage.Metric, bucket)
			//		}
			//
			//		costCells[accountId][bucket] += *usage.Cost
			//	}
			//}
		}
	}
}
