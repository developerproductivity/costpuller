# Costpuller

This tool gathers billing data for various accounts on various cloud
providers and either saves it to a local CSV file or loads it into a Google
Sheet.  It supports Amazon, Azure, Google Cloud Platform, and IBM Cloud via
Apptio Cloudability.  (In addition, the tool has the ability to pull data
directly from AWS or IBM Cloud, but this support is no longer needed.)

The configuration for this tool is provided by a YAML file.  The file
provides the list of account IDs whose spending data is to be retrieved,
grouped by cloud provider and business organization.  It also provides a
section for configuring and customizing the operation of this tool.  The file
is specified by the `-accounts` option, which uses the file `accounts.yaml`
in the current directory by default.

`go run costpuller -report /dev/null` will update the source sheet (once 
configured), and ignore the report output it generates. Use the `-help` 
option to list all command line options.


### Providing Credentials

 - Access to Cloudability is provided by either a Cloudability API Key or a
   FrontDoor/Apptio API Key-pair.  The key-pair can be obtained by logging into
   the Cloudability website, clicking on the "user" icon in the upper-right
   corner of the page, and selecting "User Profile"; then select the "API Keys"
   "tab" and click the "Create API Key" button.  Be sure to note the "Private
   Key", because it cannot be retrieved again, once you leave the pop-up -- you
   must create a new key if you lose the private portion.  The API Key is
   provided as the value of the `"api_key"` key under the `"cloudability"`
   subsection of the `"configuration"` section; alternatively, the key-pair is
   provided as a two element sequence as the value of the `"api_key_pair"` key
   in the same subsection; only provide one of the two YAML keys and omit the
   other from the file.
 - Google Sheets access is provided via OAuth 2.0.  This tool acts as an
   OAuth client.  The client configuration is provided in the conventional
   location (e.g., `${HOME}/.config/gcloud/application_default_credentials.json`
   or pointed to by the `GOOGLE_APPLICATION_CREDENTIALS` environment variable;
   see the Google [ADC documentation](https://cloud.google.com/docs/authentication/set-up-adc-local-dev-environment))
   and can be downloaded from a project on https://console.developers.google.com,
   under "Credentials".  The access token and refresh token are cached in a
   local file.  If the token file doesn't exist, this tool prompts the user to
   open a page in their browser (this should be done on the same machine
   which is running this tool).  The browser will allow the user to interact
   with Google's authentication servers, and then it will be redirected to a
   listener provided by this tool, which allows the tool to obtain the
   OAuth access code.  The tool then exchanges that for the tokens, which it
   writes to the cache file.
 - Direct AWS access is controlled in the conventional ways:  either via the
   environment variables `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` or via
   `~/.aws/` config files created by the `awscli configure` command.  If using
   the file-based credentials, you may select a specific profile, using the
   `"profile"` key in the `"aws"` subsection of the `"configuration" section
   of the accounts YAML file.
 - Access to IBM Cloud is provided via an API key.  A key may be obtained
   from the IBM Cloud web page under the "Manage" menu item "Access (IAM)"
   after logging in, by selecting "API Keys" from the sidebar and clicking on
   the "Create +" button on the right at the top of the table.  The API Key
   must be placed in the accounts YAML file as the value of the key,
   `"api_key"`, in the `"ibmcloud"` subsection of the `"configuration"`
   section.  The account ID should be set to the ID for the appropriate
   "Account Group".  The account group IDs are available from the "Accounts"
   tab in the page reached from the "Enterprise" option under the "Manage"
   menu.  The API key must have view-access to the account group itself or to
   the enterprise as whole.

### The Output

   This tool collects the billing data from the cloud provider for each
   account in the YAML file.  The data is post-processed with certain values
   being coalesced into category values.  The result is a single row for
   each account with canonical columns.  The data can be output to a CSV
   file, or it can be loaded into a Google Spreadsheet.

### The Google Sheets Spreadsheet Configuration & Magic

   The Google spreadsheet is selected by its ID which is configured in the
   `"gsheet"` subsection of the `"configuration"` section of the YAML file with
   the key, `"spreadsheetId"`.  The value comes from the URL used to view the
   spreadsheet.

   The raw data is loaded into a new "tab" or "sheet" in the spreadsheet.
   The sheet is named by expanding a name-template configured in the YAML
   file with the key `"sheetNameTemplate"`.  Digits in the value are replaced
   with elements of the reference time timestamp, as described in
   https://pkg.go.dev/time#Layout: for instance, in "Raw Data 01/2006", the
   "01" would be replaced by the two-digit numerical month and "2006" would
   be replaced by the four-digit year.  The reference time can be specified
   with the `-month` command line option, e.g. `-month 2024-08`, but it
   defaults to the month previous to the current one, which, since the data is
   published monthly, is usually the appropriate value.

   The tool expects that the spreadsheet contains a "main sheet" which
   references the raw data sheets.  This sheet must be specified in the YAML
   file using the key, `"mainSheetName"`.  Unfortunately, Google Sheets seems
   to have a mal-feature which results in situations where cells referencing
   another sheet are not updated reliably.  For instance, creating a new
   sheet or, in many cases, even just updating it, will not refresh a cell
   in another sheet which references it.  The accepted workaround for this is
   to copy and paste the cell references over themselves.  To effect
   this, the tool expects that there is a cell in the main sheet which
   contains the name of the raw data sheet and which is used for indirect
   lookups in the raw data sheet, moreover that the formulas containing the
   indirect references are found in the column immediately below this cell
   and that there is one entry for each row of data.  The tool will locate
   the cell which contains the sheet reference, copy the appropriate number
   of cells below it, and paste those values over themselves.  The paste
   operation is non-destructive, so it is not a problem if it encompasses
   unrelated cells, but it must include all cells with references to the
   new sheet.

## Acknowledgements

This tool was originally implemented by Michael Kleinhenz at 
https://github.com/michaelkleinhenz/costpuller, and the first couple of
dozen commits here are his original work.

## The configuration file format

```yaml
configuration:
  aws:
    profile: "<your-profile-name>"
  ibmcloud:
    api_key: "<your-IBM-Cloud-API-key-goes-here>"
    account_id: "<your-enterprise-account-ID>"
    cost_center: "<your-cost-center-name>"
    endpoint: "https://enterprise.cloud.ibm.com"
  cloudability:
    api: "api.cloudability.com"
    # You only need one of a Cloudability API Key or a FrontDoor/Apptio Key-pair.
    # Remove the one that you are not providing.
    api_key: "<your-Cloudabilty-API-key-goes-here>"
    api_key_pair:
      - "<your-FrontDoor/Apptio-API-keypair-ID-goes-here>"
      - "<your-FrontDoor/Apptio-API-keypair-Secret-goes-here>"
    cost_center: "<your-cost-center>"
    environmentId: "<your-Aptio-Cloudability-environment-ID>"
    filters:
      category4:  # Custom category, such as responsible cost center
        - "<value1>"
        - "<value2>"
        - ...
#        - "unknown"
      account_identifier:
        - "<payer-account-ID-1>"
        - "<payer-account-ID-2>"
        - ...
  gsheet:
    spreadsheetId: "<your-GSheet-ID>"
    mainSheetName: "Actuals FY25"
    sheetNameTemplate: "Raw Data 01/2006"  # See https://pkg.go.dev/time#Layout
  oauth:
    port: "35355"  # Arbitrary non-priv'd value
    tokenCachePath: "costpuller"

cloud_providers:
  Amazon:  # Use "aws" for direct AWS access
    "<your-team-name>":
      - accountid: "value1"
      - accountid: "value2"
      - ...
    "<another-team-name>":
      - accountid: "value1"
      - ...
    "...":
  Azure:
    ...
  GCP:
    ...
  IBM:
    ...
```
