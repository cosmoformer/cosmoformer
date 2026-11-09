// Sonar's combined metric weights covered lines and covered conditions together.
// https://docs.sonarsource.com/sonarqube-server/user-guide/code-metrics/metrics-definition
export function assessOverallCoverage(measures) {
    const values = new Map();
    for (const {metric, value} of measures) {
        if (values.has(metric)) throw Error('Duplicate coverage metric');
        values.set(metric, value);
    }
    const count = name => {
        const value = values.get(name);
        if (typeof value !== 'string' || !/^\d+$/.test(value)) throw Error(`Missing or invalid ${name}`);
        return BigInt(value);
    };
    const lines = count('lines_to_cover'), missedLines = count('uncovered_lines');
    const conditions = count('conditions_to_cover'), missedConditions = count('uncovered_conditions');
    if (!lines || missedLines > lines || missedConditions > conditions) throw Error('Invalid coverage totals');
    const coveredLines = lines - missedLines, total = lines + conditions;
    const covered = coveredLines + conditions - missedConditions;
    const linePass = coveredLines * 100n > lines * 80n;
    const combinedPass = covered * 100n > total * 80n;
    return {
        passed: linePass && combinedPass,
        lines: {covered: String(coveredLines), total: String(lines), percent: Number(coveredLines * 1000000n / lines) / 10000, passed: linePass},
        combined: {covered: String(covered), total: String(total), percent: Number(covered * 1000000n / total) / 10000, passed: combinedPass},
    };
}
