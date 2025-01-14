package logstorage

import (
	"fmt"
	"slices"
	"strings"
)

type statsRowAny struct {
	fields []string
}

func (sa *statsRowAny) String() string {
	return "row_any(" + statsFuncFieldsToString(sa.fields) + ")"
}

func (sa *statsRowAny) updateNeededFields(neededFields fieldsSet) {
	if len(sa.fields) == 0 {
		neededFields.add("*")
	} else {
		neededFields.addFields(sa.fields)
	}
}

func (sa *statsRowAny) newStatsProcessor(a *chunkedAllocator) statsProcessor {
	sap := a.newStatsRowAnyProcessor()
	sap.sa = sa
	return sap
}

type statsRowAnyProcessor struct {
	sa *statsRowAny

	captured bool

	fields []Field
}

func (sap *statsRowAnyProcessor) updateStatsForAllRows(br *blockResult) int {
	if br.rowsLen == 0 {
		return 0
	}
	if sap.captured {
		return 0
	}
	sap.captured = true

	return sap.updateState(br, 0)
}

func (sap *statsRowAnyProcessor) updateStatsForRow(br *blockResult, rowIdx int) int {
	if sap.captured {
		return 0
	}
	sap.captured = true

	return sap.updateState(br, rowIdx)
}

func (sap *statsRowAnyProcessor) mergeState(sfp statsProcessor) {
	src := sfp.(*statsRowAnyProcessor)
	if !sap.captured {
		sap.captured = src.captured
		sap.fields = src.fields
	}
}

func (sap *statsRowAnyProcessor) updateState(br *blockResult, rowIdx int) int {
	stateSizeIncrease := 0
	fields := sap.fields
	fetchFields := sap.sa.fields
	if len(fetchFields) == 0 {
		cs := br.getColumns()
		for _, c := range cs {
			v := c.getValueAtRow(br, rowIdx)
			fields = append(fields, Field{
				Name:  strings.Clone(c.name),
				Value: strings.Clone(v),
			})
			stateSizeIncrease += len(c.name) + len(v)
		}
	} else {
		for _, field := range fetchFields {
			c := br.getColumnByName(field)
			v := c.getValueAtRow(br, rowIdx)
			fields = append(fields, Field{
				Name:  strings.Clone(c.name),
				Value: strings.Clone(v),
			})
			stateSizeIncrease += len(c.name) + len(v)
		}
	}
	sap.fields = fields

	return stateSizeIncrease
}

func (sap *statsRowAnyProcessor) finalizeStats(dst []byte) []byte {
	return MarshalFieldsToJSON(dst, sap.fields)
}

func parseStatsRowAny(lex *lexer) (*statsRowAny, error) {
	if !lex.isKeyword("row_any") {
		return nil, fmt.Errorf("unexpected func; got %q; want 'row_any'", lex.token)
	}
	lex.nextToken()
	fields, err := parseFieldNamesInParens(lex)
	if err != nil {
		return nil, fmt.Errorf("cannot parse 'row_any' args: %w", err)
	}

	if slices.Contains(fields, "*") {
		fields = nil
	}

	sa := &statsRowAny{
		fields: fields,
	}
	return sa, nil
}
