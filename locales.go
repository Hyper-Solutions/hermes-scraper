package hermes

// Locales is the full set of Hermès shopping storefronts, as locale codes the
// bck.hermes.com API accepts (country_lang). Sourced from Hermès' own
// @hermes/locale package (its LOCALES list) and matching the /country-selector
// markets. The ISO aliases ae_en and sa_en from that package are omitted: they
// are the same catalogs as dh_en (UAE) and ri_en (Saudi Arabia), so scraping
// them would just re-pull an existing market.
var Locales = []string{
	"at_de", "au_en", "be_en", "be_fr", "br_pt", "ca_en", "ca_fr", "ch_de",
	"ch_fr", "cn_en", "cn_zh", "cz_en", "de_de", "dh_en", "dk_en", "es_es",
	"fi_en", "fr_fr", "gr_en", "hk_en", "ie_en", "it_it", "jp_ja", "kr_ko",
	"lu_fr", "mo_en", "mx_es", "my_en", "nl_en", "no_en", "pl_en", "pt_en",
	"ri_en", "se_en", "sg_en", "th_en", "tw_zh", "uk_en", "us_en",
}
