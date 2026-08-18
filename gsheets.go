package main

import (
	"cmp"
	"context"
	"fmt"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
)

type providerAccountMetadata struct {
	AccountName    string
	CloudProvider  string
	CostCenter     string
	Date           string
	PayerAccountId string
}

// postToGSheet creates a new sheet in a Google Sheets spreadsheet and loads it
// with the specified data.  Requests are made to the Google API using the
// specified HTTP client which has already been authenticated and authorized.
// The new sheet name is constructed based on the reference time passed in the
// last parameter.  Details such as the spreadsheet ID and sheet names are found
// in the configuration map.
func postToGSheet(sheetData []*sheets.RowData, client *http.Client, configMap Configuration, ref time.Time) {
	srv, err := sheets.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		log.Fatalf("Unable to create Google Sheets client: %v", err)
	}

	// Construct the name for the raw data sheet using the template-name from
	// the configuration as a format specifier for time.Format()
	// (see https://pkg.go.dev/time#Layout).  Format fields (represented by
	// strings of digits) are replaced with portions of the reference time
	// value while non-digits are copied literally, so, if the template-name is
	// "Raw Data 01/2006" and the reference time is in August 2024, the result
	// will be "Raw Data 08/2024".
	newSheetName := ref.Format(getMapKeyString(configMap, "sheetNameTemplate", "gsheet"))

	spreadsheetId := getMapKeyString(configMap, "spreadsheetId", "gsheet")
	log.Println("Fetching Spreadsheet information")
	sheetObject, err := srv.Spreadsheets.
		Get(spreadsheetId).
		Fields("sheets/properties(gridProperties(columnCount,rowCount),sheetId,title)", "spreadsheetId").
		Do()
	if err != nil {
		log.Fatalf("Error retrieving spreadsheet: %v", err)
	}

	newDataRef := getUpdateLocation(srv, sheetObject, newSheetName, len(sheetData[0].Values), len(sheetData))

	mainSheetName := getMapKeyString(configMap, "mainSheetName", "gsheet")
	mainSheetProperties := getSheetIdFromName(sheetObject, mainSheetName)
	if mainSheetProperties == nil {
		log.Fatalf("Error updating spreadsheet sheet: main sheet %q not found", mainSheetName)
	}
	mainSheetID := mainSheetProperties.SheetId
	cells, err := srv.Spreadsheets.Values.Get(spreadsheetId, fmt.Sprintf(
		"'%s'!A1:%s%d",
		mainSheetName,
		colNumToRef(int(mainSheetProperties.GridProperties.ColumnCount-1)), // Index of last column
		mainSheetProperties.GridProperties.RowCount,
	)).Do()
	if err != nil {
		log.Fatalf("Error fetching main sheet (%q) values: %v", mainSheetID, err)
	}
	// Increase the length by one to cover the "Total" row
	mainSheetRef := getNewSheetReference(cells, mainSheetID, newSheetName, len(sheetData)+1)
	if mainSheetRef == nil {
		log.Fatalf("No reference to %q found in main sheet (%q)", newSheetName, mainSheetName)
	}
	loadNewData(srv, spreadsheetId, sheetData, newDataRef, mainSheetRef)
}

// getUpdateLocation is a helper function which returns the GridRange to
// receive the new data.  This includes looking up the existing sheet or
// creating a new one with the indicated number of columns and rows.
func getUpdateLocation(
	srv *sheets.Service,
	sheetObject *sheets.Spreadsheet,
	newSheetName string,
	newColumnCount int,
	newRowCount int,
) (newDataRef *sheets.GridRange) {
	newSheetProperties := getSheetIdFromName(sheetObject, newSheetName)
	if newSheetProperties == nil {
		log.Printf("Adding new sheet %q", newSheetName)
		spreadsheetId := sheetObject.SpreadsheetId
		newSheetProperties = createNewSheet(
			srv,
			spreadsheetId,
			newSheetName,
			int64(len(sheetObject.Sheets)), // Insert the sheet at the end
			int64(newColumnCount),
			int64(newRowCount),
		)
	} else {
		log.Printf("Warning:  overwriting sheet %q", newSheetName)
		if int64(newRowCount) > newSheetProperties.GridProperties.RowCount ||
			int64(newColumnCount) > newSheetProperties.GridProperties.ColumnCount {
			rows := newSheetProperties.GridProperties.RowCount
			cols := newSheetProperties.GridProperties.ColumnCount
			if int64(newRowCount) > rows {
				rows = int64(newRowCount)
			}
			if int64(newColumnCount) > cols {
				cols = int64(newColumnCount)
			}
			log.Printf("Resizing sheet %q from %dx%d to %dx%d",
				newSheetName,
				newSheetProperties.GridProperties.RowCount,
				newSheetProperties.GridProperties.ColumnCount,
				rows, cols)
			_, err := srv.Spreadsheets.BatchUpdate(sheetObject.SpreadsheetId,
				&sheets.BatchUpdateSpreadsheetRequest{
					Requests: []*sheets.Request{
						{
							UpdateSheetProperties: &sheets.UpdateSheetPropertiesRequest{
								Fields: "gridProperties(rowCount,columnCount)",
								Properties: &sheets.SheetProperties{
									SheetId: newSheetProperties.SheetId,
									GridProperties: &sheets.GridProperties{
										RowCount:    rows,
										ColumnCount: cols,
									},
								},
							},
						},
					},
				}).Do()
			if err != nil {
				log.Fatalf("Error resizing sheet %q: %v", newSheetName, err)
			}
			newSheetProperties.GridProperties.RowCount = rows
			newSheetProperties.GridProperties.ColumnCount = cols
		}
	}
	return getDataGridRange(newSheetProperties)
}

// loadNewData updates the data cells (avoiding the header row and the totals
// column) in the indicated sheet of the indicated spreadsheet from the
// provided RowData using the provided service client; it then copies a range
// of cells new sheet with the new data, and then poke the main sheet
// to get it to update its references to the new sheet.
func loadNewData(
	srv *sheets.Service,
	spreadsheetId string,
	sheetData []*sheets.RowData,
	newSheetRef *sheets.GridRange,
	mainSheetRef *sheets.GridRange,
) {
	response, err := srv.Spreadsheets.BatchUpdate(spreadsheetId, &sheets.BatchUpdateSpreadsheetRequest{
		Requests: []*sheets.Request{
			{
				UpdateCells: &sheets.UpdateCellsRequest{
					Fields: "userEnteredValue,userEnteredFormat",
					Range:  newSheetRef,
					Rows:   sheetData,
				},
			},
			{
				CopyPaste: &sheets.CopyPasteRequest{
					Destination:      mainSheetRef,
					PasteOrientation: "NORMAL",
					PasteType:        "PASTE_NORMAL",
					Source:           mainSheetRef,
				},
			},
		},
	}).Do()
	if err != nil {
		log.Fatalf("Error updating sheet: %v, [%v]", err, response)
	}
	// Auto-resizing the columns doesn't work well until after the data has
	// been updated (and, even then, it seems about 10% too narrow on my
	// screen), so this needs to be done in a separate request.
	response, err = srv.Spreadsheets.BatchUpdate(spreadsheetId, &sheets.BatchUpdateSpreadsheetRequest{
		Requests: []*sheets.Request{
			{
				AutoResizeDimensions: &sheets.AutoResizeDimensionsRequest{
					Dimensions: &sheets.DimensionRange{
						Dimension: "COLUMNS",
						SheetId:   newSheetRef.SheetId,
					},
				},
			},
		},
	}).Do()
	if err != nil {
		log.Fatalf("Error updating column widths again: %v, [%v]", err, response)
	}
}

// createNewSheet creates a new sheet with the provided number of columns and
// rows in the provided spreadsheet using the provided service client inserting
// it into the spreadsheet at the indicated position with the provided name; it
// then returns a pointer to the resulting sheet's properties.
func createNewSheet(
	srv *sheets.Service,
	spreadsheetId string,
	newSheetName string,
	position int64,
	columnCount int64,
	rowCount int64,
) *sheets.SheetProperties {
	buResp, err := srv.Spreadsheets.BatchUpdate(spreadsheetId, &sheets.BatchUpdateSpreadsheetRequest{
		Requests: []*sheets.Request{
			{
				AddSheet: &sheets.AddSheetRequest{
					Properties: &sheets.SheetProperties{
						GridProperties: &sheets.GridProperties{
							ColumnCount: columnCount,
							RowCount:    rowCount,
						},
						Hidden: true,
						Index:  position,
						Title:  newSheetName,
					},
				},
			},
		},
	}).Do()
	if err != nil {
		log.Fatalf("Error creating sheet: %v", err)
	}

	return buResp.Replies[0].AddSheet.Properties
}

// getGridRange is a helper function which, given the sheet's properties
// object, produces a GridRange describing the whole sheet.
func getDataGridRange(props *sheets.SheetProperties) *sheets.GridRange {
	return &sheets.GridRange{
		EndColumnIndex:   props.GridProperties.ColumnCount,
		EndRowIndex:      props.GridProperties.RowCount,
		SheetId:          props.SheetId,
		StartColumnIndex: 0,
		StartRowIndex:    0,
	}
}

// getNewSheetReference returns a pointer to a GridRange which describes the
// cells in the provided main sheet which (indirectly) refer to the indicated
// new sheet.  This is done by locating the cell in the provided ValueRange
// which refers to the provided new sheet by name; we assume the indirect
// references are in the same column starting in the row below the matching
// cell and that there will be the provided number of rows.
func getNewSheetReference(
	cells *sheets.ValueRange,
	mainSheetID int64,
	newSheetName string,
	rowCount int,
) *sheets.GridRange {
	for r, row := range cells.Values {
		for c, cell := range row {
			if str, ok := cell.(string); ok {
				if strings.Contains(str, newSheetName) {
					msColumn := int64(c)
					msRow := int64(r + 1)
					// Indices are zero-based, starts are inclusive, ends are exclusive.
					return &sheets.GridRange{
						EndColumnIndex:   msColumn + 1,
						EndRowIndex:      msRow + int64(rowCount) + 1,
						SheetId:          mainSheetID,
						StartColumnIndex: msColumn,
						StartRowIndex:    msRow,
					}
				}
			}
		}
	}
	return nil
}

// getSheetIdFromName is a helper function which returns the sheet properties
// object for the sheet (tab) with the given name in the specified spreadsheet,
// or nil if the sheet was not found.
func getSheetIdFromName(sheetObject *sheets.Spreadsheet, sheetName string) *sheets.SheetProperties {
	for _, sheet := range sheetObject.Sheets {
		if sheet.Properties.Title == sheetName {
			return sheet.Properties
		}
	}
	return nil
}

func newStringCell(val string) *sheets.CellData {
	return &sheets.CellData{UserEnteredValue: &sheets.ExtendedValue{StringValue: &val}}
}

func newNumberCell(val float64) *sheets.CellData {
	return &sheets.CellData{UserEnteredValue: &sheets.ExtendedValue{NumberValue: &val}}
}

func newFormulaCell(formula string) *sheets.CellData {
	return &sheets.CellData{
		UserEnteredValue: &sheets.ExtendedValue{
			FormulaValue: &formula,
		},
	}
}

// getSheetFromCostCells converts the cost data into a Google Sheet.
func getSheetFromCostCells(
	costCells map[string]map[string]float64,
	columnHeadsSet map[string]struct{},
	accountsMetadata map[string]*AccountMetadata,
	metadata map[string]providerAccountMetadata,
) (output []*sheets.RowData) {
	// Build a list of column headers, starting with a fixed set of strings for
	// metadata and ending with the headers collected from the data.
	//
	// Note:  The "Account ID" column will be used as the key for lookups, so
	// it must appear before any values (such as the totals) which will be
	// looked up.
	columnHeadsList := []string{"Team", "Date", "Cloud Provider", "Payer ID",
		"Cost Center", "Account Name", "Account ID", "TOTAL"}
	fixed := len(columnHeadsList)
	columnHeadsList = append(columnHeadsList, sortedKeys(columnHeadsSet)...)

	// Add the headers to the sheet data as the first row.
	sheetRow := make([]*sheets.CellData, len(columnHeadsList))
	for idx, header := range columnHeadsList {
		sheetRow[idx] = newStringCell(header)
		sheetRow[idx].UserEnteredFormat = &sheets.CellFormat{
			BackgroundColorStyle: &sheets.ColorStyle{
				RgbColor: &sheets.Color{
					Blue:  204.0 / 256.0,
					Green: 204.0 / 256.0,
					Red:   204.0 / 256.0,
				},
			},
			HorizontalAlignment: "CENTER",
			TextFormat:          &sheets.TextFormat{Bold: true},
		}
	}
	output = append(output, &sheets.RowData{Values: sheetRow})

	// Fill in the sheet with one row for each account, iterating over the
	// column headers and inserting the appropriate values into each cell.
	for accountId, dataRow := range costCells {
		sheetRow = make([]*sheets.CellData, len(columnHeadsList))
		for idx, key := range columnHeadsList {
			var val *sheets.CellData
			switch {
			case key == "TOTAL":
				val = nil // Will be set after sorting
			case key == "Team":
				val = newStringCell(accountsMetadata[accountId].Group)
			case key == "Date":
				val = newStringCell(metadata[accountId].Date)
			case key == "Cloud Provider":
				val = newStringCell(accountsMetadata[accountId].CloudProvider)
			case key == "Cost Center":
				val = newStringCell(metadata[accountId].CostCenter)
			case key == "Payer ID":
				val = newStringCell(metadata[accountId].PayerAccountId)
			case key == "Account ID": // Use the ID from the YAML file, not from Cloudability
				val = newStringCell(accountsMetadata[accountId].AccountId)
			case key == "Account Name":
				val = newStringCell(metadata[accountId].AccountName)
			default:
				val = newNumberCell(dataRow[key])
				val.UserEnteredFormat = &sheets.CellFormat{
					NumberFormat: &sheets.NumberFormat{
						//Pattern: "",
						Type: "CURRENCY",
					},
				}
			}
			sheetRow[idx] = val
		}
		output = append(output, &sheets.RowData{Values: sheetRow})
	}

	sortOutput(output[1:], slices.Index(columnHeadsList, "Account ID"))
	sortOutput(output[1:], slices.Index(columnHeadsList, "Cloud Provider"))
	sortOutput(output[1:], slices.Index(columnHeadsList, "Team"))

	// Now that we have the grid sorted, set the "TOTAL" formulas, each of
	// which has to be relative to its own row (so, sorting screws them up).
	tc := slices.Index(columnHeadsList, "TOTAL")
	for idx, row := range output[1:] {
		row.Values[tc] = newFormulaCell(getTotalsFormula(idx+1, fixed, len(columnHeadsList)-1))
		row.Values[tc].UserEnteredFormat = &sheets.CellFormat{
			BackgroundColorStyle: &sheets.ColorStyle{
				RgbColor: &sheets.Color{
					Blue:  239.0 / 256.0,
					Green: 239.0 / 256.0,
					Red:   239.0 / 256.0,
				},
			},
		}
	}

	return
}

// sortOutput sorts the rows of the provided sheet according to the indicated
// column.  Uses a stable sort so that we can retain the ordering from previous
// sorts for entries with equal values in the current sort.
func sortOutput(output []*sheets.RowData, columnIndex int) {
	slices.SortStableFunc(output, func(a, b *sheets.RowData) int {
		return cmp.Compare(
			*a.Values[columnIndex].UserEnteredValue.StringValue,
			*b.Values[columnIndex].UserEnteredValue.StringValue)
	})
}

// getTotalsFormula is a helper function which constructs a formula for
// calculating the sum of a consecutive range of values a row of a sheet.
// The arguments are the index of the row to sum, the column of the first
// value, and the column of the last value -- the indices are zero-based.
// The references are converted to A1:B1 form.
func getTotalsFormula(row int, startCol int, endCol int) string {
	return fmt.Sprintf(
		"=SUM(%s%d:%s%d)",
		colNumToRef(startCol),
		row+1,
		colNumToRef(endCol),
		row+1,
	)
}

// colNumToRef converts a zero-based column ordinal to a letter-reference
// (e.g., 0 yields "A"; 25 yields "Z"; 26 yields "AA"; 676 yields "AAA").
func colNumToRef(n int) (s string) {
	d, r := n/26, n%26
	if d > 0 {
		s = colNumToRef(d - 1)
	}
	return s + fmt.Sprintf("%c", 'A'+r)
}
