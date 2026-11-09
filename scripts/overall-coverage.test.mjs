import {test} from 'node:test';
import assert from 'node:assert/strict';
import {assessOverallCoverage} from './overall-coverage.mjs';
const measures = (lines, uncovered, conditions = 0, missed = 0) => Object.entries({lines_to_cover: lines, uncovered_lines: uncovered, conditions_to_cover: conditions, uncovered_conditions: missed}).map(([metric, value]) => ({metric, value: String(value)}));
test('both overall measures must strictly exceed 80% using exact weighted totals', () => {
    assert.equal(assessOverallCoverage(measures(100, 20)).passed, false);
    assert.equal(assessOverallCoverage(measures(100, 19)).passed, true);
    assert.equal(assessOverallCoverage(measures(100, 19, 100, 21)).passed, false);
    assert.equal(assessOverallCoverage(measures(100, 21, 100, 0)).passed, false);
    const result = assessOverallCoverage(measures(100, 10, 100, 20));
    assert.equal(result.passed, true);
    assert.deepEqual(result.combined, {covered: '170', total: '200', percent: 85, passed: true});
});
test('rounding and averages cannot turn a failing count into a pass', () => {
    assert.equal(assessOverallCoverage(measures(8694, 1739)).passed, false);
    assert.equal(assessOverallCoverage(measures(8694, 1737)).passed, true);
    assert.equal(assessOverallCoverage(measures(1000000000000000001n, 200000000000000000n)).passed, true);
    assert.equal(assessOverallCoverage(measures(1000000000000000000n, 200000000000000001n)).passed, false);
});
test('rejects missing, duplicated, negative, fractional and impossible metric data', () => {
    for (const input of [[], measures(0, 0), measures(10, 11), measures(10, 0, 2, 3), measures(-1, 0), measures('NaN', 0), measures('1.5', 0), measures(10, 0).slice(1), [...measures(10, 0), {metric: 'lines_to_cover', value: '10'}]]) assert.throws(() => assessOverallCoverage(input));
});
