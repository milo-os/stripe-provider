// SPDX-License-Identifier: AGPL-3.0-only

// Package taxids carries the curated metadata stripe-provider
// publishes onto StripeProviderConfig.status.supportedTaxIDTypes.
//
// The Stripe SDK exposes the wire-format vocabulary as
// `stripe.TaxIDType` constants but does not ship human-readable display
// names, example formats, or country mappings. We curate those here so
// the portal can drive the Billing Address & Tax IDs form without
// hardcoding the Stripe vocabulary. The list mirrors stripe-go v81's
// tax_id_data.type values 1:1 and is updated when the SDK is bumped.
//
// When stripe-go ships a new tax ID type and we forget to add metadata
// for it, the portal will still see the type code via status — just
// without a friendly label or example.
package taxids

// Entry is one supported tax-ID type. Field meanings match
// stripev1alpha1.SupportedTaxIDType.
type Entry struct {
	Type        string
	DisplayName string
	Example     string
	Country     string
}

// All returns the curated metadata table. The slice is sorted by Type
// so the published status is deterministic across reconciles.
func All() []Entry {
	out := make([]Entry, len(entries))
	copy(out, entries)
	return out
}

// entries is the canonical list. Ordered alphabetically by Type so
// the published status is stable across SDK bumps; controllers that
// publish via .status see the same list as readers iterating the
// dropdown.
//
// Country is the ISO 3166-1 alpha-2 country code of the issuing
// jurisdiction, or "EU" for the two cross-jurisdictional types
// (eu_vat, eu_oss_vat). Example values are taken from Stripe's
// documentation; they're illustrative, not validating regex.
var entries = []Entry{
	{Type: "ad_nrt", DisplayName: "Andorra NRT", Example: "A-123456-Z", Country: "AD"},
	{Type: "ae_trn", DisplayName: "United Arab Emirates TRN", Example: "123456789012345", Country: "AE"},
	{Type: "al_tin", DisplayName: "Albania TIN", Example: "J12345678N", Country: "AL"},
	{Type: "am_tin", DisplayName: "Armenia TIN", Example: "02538904", Country: "AM"},
	{Type: "ao_tin", DisplayName: "Angola TIN", Example: "5123456789", Country: "AO"},
	{Type: "ar_cuit", DisplayName: "Argentina CUIT", Example: "12-3456789-01", Country: "AR"},
	{Type: "au_abn", DisplayName: "Australia ABN", Example: "12345678912", Country: "AU"},
	{Type: "au_arn", DisplayName: "Australia ARN", Example: "1234567891234", Country: "AU"},
	{Type: "ba_tin", DisplayName: "Bosnia and Herzegovina TIN", Example: "123456789012", Country: "BA"},
	{Type: "bb_tin", DisplayName: "Barbados TIN", Example: "1123456789012", Country: "BB"},
	{Type: "bg_uic", DisplayName: "Bulgaria UIC", Example: "123456789", Country: "BG"},
	{Type: "bh_vat", DisplayName: "Bahrain VAT", Example: "123456789012345", Country: "BH"},
	{Type: "bo_tin", DisplayName: "Bolivia TIN", Example: "1234567899", Country: "BO"},
	{Type: "br_cnpj", DisplayName: "Brazil CNPJ", Example: "01.234.456/5432-10", Country: "BR"},
	{Type: "br_cpf", DisplayName: "Brazil CPF", Example: "123.456.789-87", Country: "BR"},
	{Type: "bs_tin", DisplayName: "Bahamas TIN", Example: "123456789", Country: "BS"},
	{Type: "by_tin", DisplayName: "Belarus TIN", Example: "123456789", Country: "BY"},
	{Type: "ca_bn", DisplayName: "Canada BN", Example: "123456789", Country: "CA"},
	{Type: "ca_gst_hst", DisplayName: "Canada GST/HST", Example: "123456789RT0002", Country: "CA"},
	{Type: "ca_pst_bc", DisplayName: "Canada PST (British Columbia)", Example: "PST-1234-5678", Country: "CA"},
	{Type: "ca_pst_mb", DisplayName: "Canada PST (Manitoba)", Example: "123456-7", Country: "CA"},
	{Type: "ca_pst_sk", DisplayName: "Canada PST (Saskatchewan)", Example: "1234567", Country: "CA"},
	{Type: "ca_qst", DisplayName: "Canada QST", Example: "1234567890TQ1234", Country: "CA"},
	{Type: "cd_nif", DisplayName: "DR Congo NIF", Example: "A0123456M", Country: "CD"},
	{Type: "ch_uid", DisplayName: "Switzerland UID", Example: "CHE-123.456.788", Country: "CH"},
	{Type: "ch_vat", DisplayName: "Switzerland VAT", Example: "CHE-123.456.789 MWST", Country: "CH"},
	{Type: "cl_tin", DisplayName: "Chile TIN", Example: "12.345.678-K", Country: "CL"},
	{Type: "cn_tin", DisplayName: "China TIN", Example: "123456789012345678", Country: "CN"},
	{Type: "co_nit", DisplayName: "Colombia NIT", Example: "123.456.789-0", Country: "CO"},
	{Type: "cr_tin", DisplayName: "Costa Rica TIN", Example: "1-234-567890", Country: "CR"},
	{Type: "de_stn", DisplayName: "Germany Steuernummer", Example: "1234567890", Country: "DE"},
	{Type: "do_rcn", DisplayName: "Dominican Republic RCN", Example: "123-4567890-1", Country: "DO"},
	{Type: "ec_ruc", DisplayName: "Ecuador RUC", Example: "1234567890001", Country: "EC"},
	{Type: "eg_tin", DisplayName: "Egypt TIN", Example: "123456789", Country: "EG"},
	{Type: "es_cif", DisplayName: "Spain CIF", Example: "A12345678", Country: "ES"},
	{Type: "eu_oss_vat", DisplayName: "EU One Stop Shop VAT", Example: "EU123456789", Country: "EU"},
	{Type: "eu_vat", DisplayName: "EU VAT", Example: "DE123456789", Country: "EU"},
	{Type: "gb_vat", DisplayName: "United Kingdom VAT", Example: "GB123456789", Country: "GB"},
	{Type: "ge_vat", DisplayName: "Georgia VAT", Example: "123456789", Country: "GE"},
	{Type: "gn_nif", DisplayName: "Guinea NIF", Example: "123456789", Country: "GN"},
	{Type: "hk_br", DisplayName: "Hong Kong BR", Example: "12345678", Country: "HK"},
	{Type: "hr_oib", DisplayName: "Croatia OIB", Example: "12345678901", Country: "HR"},
	{Type: "hu_tin", DisplayName: "Hungary TIN", Example: "12345678-1-23", Country: "HU"},
	{Type: "id_npwp", DisplayName: "Indonesia NPWP", Example: "012345678901234", Country: "ID"},
	{Type: "il_vat", DisplayName: "Israel VAT", Example: "000012345", Country: "IL"},
	{Type: "in_gst", DisplayName: "India GST", Example: "12ABCDE3456FGZH", Country: "IN"},
	{Type: "is_vat", DisplayName: "Iceland VAT", Example: "123456", Country: "IS"},
	{Type: "jp_cn", DisplayName: "Japan Corporate Number", Example: "1234567891234", Country: "JP"},
	{Type: "jp_rn", DisplayName: "Japan Registered Number", Example: "12345", Country: "JP"},
	{Type: "jp_trn", DisplayName: "Japan TRN", Example: "T1234567891234", Country: "JP"},
	{Type: "ke_pin", DisplayName: "Kenya PIN", Example: "P000111111A", Country: "KE"},
	{Type: "kh_tin", DisplayName: "Cambodia TIN", Example: "1001-123456789", Country: "KH"},
	{Type: "kr_brn", DisplayName: "South Korea BRN", Example: "123-45-67890", Country: "KR"},
	{Type: "kz_bin", DisplayName: "Kazakhstan BIN", Example: "123456789012", Country: "KZ"},
	{Type: "li_uid", DisplayName: "Liechtenstein UID", Example: "CHE123456789", Country: "LI"},
	{Type: "li_vat", DisplayName: "Liechtenstein VAT", Example: "12345", Country: "LI"},
	{Type: "ma_vat", DisplayName: "Morocco VAT", Example: "12345678", Country: "MA"},
	{Type: "md_vat", DisplayName: "Moldova VAT", Example: "1234567", Country: "MD"},
	{Type: "me_pib", DisplayName: "Montenegro PIB", Example: "12345678", Country: "ME"},
	{Type: "mk_vat", DisplayName: "North Macedonia VAT", Example: "MK1234567890123", Country: "MK"},
	{Type: "mr_nif", DisplayName: "Mauritania NIF", Example: "12345678", Country: "MR"},
	{Type: "mx_rfc", DisplayName: "Mexico RFC", Example: "ABC010203AB9", Country: "MX"},
	{Type: "my_frp", DisplayName: "Malaysia FRP", Example: "12345678", Country: "MY"},
	{Type: "my_itn", DisplayName: "Malaysia ITN", Example: "C 1234567890", Country: "MY"},
	{Type: "my_sst", DisplayName: "Malaysia SST", Example: "A12-3456-78912345", Country: "MY"},
	{Type: "ng_tin", DisplayName: "Nigeria TIN", Example: "12345678-0001", Country: "NG"},
	{Type: "no_vat", DisplayName: "Norway VAT", Example: "123456789MVA", Country: "NO"},
	{Type: "no_voec", DisplayName: "Norway VOEC", Example: "1234567", Country: "NO"},
	{Type: "np_pan", DisplayName: "Nepal PAN", Example: "123456789", Country: "NP"},
	{Type: "nz_gst", DisplayName: "New Zealand GST", Example: "123456789", Country: "NZ"},
	{Type: "om_vat", DisplayName: "Oman VAT", Example: "OM1234567890", Country: "OM"},
	{Type: "pe_ruc", DisplayName: "Peru RUC", Example: "12345678901", Country: "PE"},
	{Type: "ph_tin", DisplayName: "Philippines TIN", Example: "123456789", Country: "PH"},
	{Type: "ro_tin", DisplayName: "Romania TIN", Example: "1234567890123", Country: "RO"},
	{Type: "rs_pib", DisplayName: "Serbia PIB", Example: "123456789", Country: "RS"},
	{Type: "ru_inn", DisplayName: "Russia INN", Example: "1234567891", Country: "RU"},
	{Type: "ru_kpp", DisplayName: "Russia KPP", Example: "123456789", Country: "RU"},
	{Type: "sa_vat", DisplayName: "Saudi Arabia VAT", Example: "123456789012345", Country: "SA"},
	{Type: "sg_gst", DisplayName: "Singapore GST", Example: "M12345678X", Country: "SG"},
	{Type: "sg_uen", DisplayName: "Singapore UEN", Example: "123456789F", Country: "SG"},
	{Type: "si_tin", DisplayName: "Slovenia TIN", Example: "12345678", Country: "SI"},
	{Type: "sn_ninea", DisplayName: "Senegal NINEA", Example: "12345672A2", Country: "SN"},
	{Type: "sr_fin", DisplayName: "Suriname FIN", Example: "1234567890", Country: "SR"},
	{Type: "sv_nit", DisplayName: "El Salvador NIT", Example: "1234-567890-123-4", Country: "SV"},
	{Type: "th_vat", DisplayName: "Thailand VAT", Example: "1234567891234", Country: "TH"},
	{Type: "tj_tin", DisplayName: "Tajikistan TIN", Example: "123456789", Country: "TJ"},
	{Type: "tr_tin", DisplayName: "Turkey TIN", Example: "0123456789", Country: "TR"},
	{Type: "tw_vat", DisplayName: "Taiwan VAT", Example: "12345678", Country: "TW"},
	{Type: "tz_vat", DisplayName: "Tanzania VAT", Example: "12345678A", Country: "TZ"},
	{Type: "ua_vat", DisplayName: "Ukraine VAT", Example: "123456789", Country: "UA"},
	{Type: "ug_tin", DisplayName: "Uganda TIN", Example: "1234567890", Country: "UG"},
	{Type: "us_ein", DisplayName: "United States EIN", Example: "12-3456789", Country: "US"},
	{Type: "uy_ruc", DisplayName: "Uruguay RUC", Example: "123456789012", Country: "UY"},
	{Type: "uz_tin", DisplayName: "Uzbekistan TIN", Example: "123456789", Country: "UZ"},
	{Type: "uz_vat", DisplayName: "Uzbekistan VAT", Example: "123456789012", Country: "UZ"},
	{Type: "ve_rif", DisplayName: "Venezuela RIF", Example: "A-12345678-9", Country: "VE"},
	{Type: "vn_tin", DisplayName: "Vietnam TIN", Example: "1234567890", Country: "VN"},
	{Type: "za_vat", DisplayName: "South Africa VAT", Example: "4123456789", Country: "ZA"},
	{Type: "zm_tin", DisplayName: "Zambia TIN", Example: "1004751879", Country: "ZM"},
	{Type: "zw_tin", DisplayName: "Zimbabwe TIN", Example: "1234567890", Country: "ZW"},
}
