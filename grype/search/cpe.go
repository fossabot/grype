package search

import (
	"fmt"
	"sort"

	"github.com/facebookincubator/nvdtools/wfn"
	"github.com/scylladb/go-set/strset"

	"github.com/anchore/grype/grype/match"
	"github.com/anchore/grype/grype/pkg"
	"github.com/anchore/grype/grype/version"
	"github.com/anchore/grype/grype/vulnerability"
	syftPkg "github.com/anchore/syft/syft/pkg"
)

type CPEParameters struct {
	Namespace string   `json:"namespace"`
	CPEs      []string `json:"cpes"`
}

func (i *CPEParameters) Merge(other CPEParameters) error {
	if i.Namespace != other.Namespace {
		return fmt.Errorf("namespaces do not match")
	}

	existingCPEs := strset.New(i.CPEs...)
	newCPEs := strset.New(other.CPEs...)
	mergedCPEs := strset.Union(existingCPEs, newCPEs).List()
	sort.Strings(mergedCPEs)
	i.CPEs = mergedCPEs
	return nil
}

type CPEResult struct {
	VersionConstraint string   `json:"versionConstraint"`
	CPEs              []string `json:"cpes"`
}

func (h CPEResult) Equals(other CPEResult) bool {
	if h.VersionConstraint != other.VersionConstraint {
		return false
	}

	if len(h.CPEs) != len(other.CPEs) {
		return false
	}

	for i := range h.CPEs {
		if h.CPEs[i] != other.CPEs[i] {
			return false
		}
	}

	return true
}

// ByPackageCPE retrieves all vulnerabilities that match the generated CPE
func ByPackageCPE(store vulnerability.ProviderByCPE, p pkg.Package, upstreamMatcher match.MatcherType) ([]match.Match, error) {
	// we attempt to merge match details within the same matcher when searching by CPEs, in this way there are fewer duplicated match
	// objects (and fewer duplicated match details).
	matchesByFingerprint := make(map[match.Fingerprint]match.Match)
	for _, cpe := range p.CPEs {
		// prefer the CPE version, but if npt specified use the package version
		searchVersion := cpe.Version
		if searchVersion == wfn.NA || searchVersion == wfn.Any {
			searchVersion = p.Version
		}
		verObj, err := version.NewVersion(searchVersion, version.FormatFromPkgType(p.Type))
		if err != nil {
			return nil, fmt.Errorf("matcher failed to parse version pkg=%q ver=%q: %w", p.Name, p.Version, err)
		}

		// find all vulnerability records in the DB for the given CPE (not including version comparisons)
		allPkgVulns, err := store.GetByCPE(cpe)
		if err != nil {
			return nil, fmt.Errorf("matcher failed to fetch by CPE pkg=%q: %w", p.Name, err)
		}

		applicableVulns, err := onlyVulnerableVersions(verObj, allPkgVulns)
		if err != nil {
			return nil, fmt.Errorf("unable to filter cpe-related vulnerabilities: %w", err)
		}

		applicableVulns = onlyVulnerableTargets(p, applicableVulns)

		// for each vulnerability record found, check the version constraint. If the constraint is satisfied
		// relative to the current version information from the CPE (or the package) then the given package
		// is vulnerable.
		for _, vuln := range applicableVulns {
			addNewMatch(matchesByFingerprint, vuln, p, *verObj, upstreamMatcher, cpe)
		}
	}

	return toMatches(matchesByFingerprint), nil
}

func addNewMatch(matchesByFingerprint map[match.Fingerprint]match.Match, vuln vulnerability.Vulnerability, p pkg.Package, searchVersion version.Version, upstreamMatcher match.MatcherType, searchedByCPE syftPkg.CPE) {
	candidateMatch := match.Match{

		Vulnerability: vuln,
		Package:       p,
	}

	if existingMatch, exists := matchesByFingerprint[candidateMatch.Fingerprint()]; exists {
		candidateMatch = existingMatch
	}

	candidateMatch.Details = addMatchDetails(candidateMatch.Details,
		match.Detail{
			Type:       match.CPEMatch,
			Confidence: 0.9, // TODO: this is hard coded for now
			Matcher:    upstreamMatcher,
			SearchedBy: CPEParameters{
				Namespace: vuln.Namespace,
				CPEs: []string{
					searchedByCPE.BindToFmtString(),
				},
			},
			Found: CPEResult{
				VersionConstraint: vuln.Constraint.String(),
				CPEs:              cpesToString(filterCPEsByVersion(searchVersion, vuln.CPEs)),
			},
		},
	)

	matchesByFingerprint[candidateMatch.Fingerprint()] = candidateMatch
}

func addMatchDetails(existingDetails []match.Detail, newDetails match.Detail) []match.Detail {
	newFound, ok := newDetails.Found.(CPEResult)
	if !ok {
		return existingDetails
	}

	newSearchedBy, ok := newDetails.SearchedBy.(CPEParameters)
	if !ok {
		return existingDetails
	}
	for idx, detail := range existingDetails {
		found, ok := detail.Found.(CPEResult)
		if !ok {
			continue
		}

		searchedBy, ok := detail.SearchedBy.(CPEParameters)
		if !ok {
			continue
		}

		if !found.Equals(newFound) {
			continue
		}

		err := searchedBy.Merge(newSearchedBy)
		if err != nil {
			continue
		}

		existingDetails[idx].SearchedBy = searchedBy
		return existingDetails
	}

	// could not merge with another entry, append to the end
	existingDetails = append(existingDetails, newDetails)
	return existingDetails
}

func filterCPEsByVersion(pkgVersion version.Version, allCPEs []syftPkg.CPE) (matchedCPEs []syftPkg.CPE) {
	for _, c := range allCPEs {
		if c.Version == wfn.Any || c.Version == wfn.NA {
			matchedCPEs = append(matchedCPEs, c)
			continue
		}

		constraint, err := version.GetConstraint(c.Version, version.UnknownFormat)
		if err != nil {
			// if we can't get a version constraint, don't filter out the CPE
			matchedCPEs = append(matchedCPEs, c)
			continue
		}

		satisfied, err := constraint.Satisfied(&pkgVersion)
		if err != nil || satisfied {
			// if we can't check for version satisfaction, don't filter out the CPE
			matchedCPEs = append(matchedCPEs, c)
			continue
		}
	}
	return matchedCPEs
}

func toMatches(matchesByFingerprint map[match.Fingerprint]match.Match) (matches []match.Match) {
	for _, m := range matchesByFingerprint {
		matches = append(matches, m)
	}
	sort.Sort(match.ByElements(matches))
	return matches
}

// cpesToString receives one or more CPEs and stringifies them
func cpesToString(cpes []syftPkg.CPE) []string {
	var strs = make([]string, len(cpes))
	for idx, cpe := range cpes {
		strs[idx] = cpe.BindToFmtString()
	}
	sort.Strings(strs)
	return strs
}
