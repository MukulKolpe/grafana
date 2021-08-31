import { size } from 'lodash';
import { QueryHint, QueryFix, TimeRange, rangeUtil } from '@grafana/data';
import { PrometheusDatasource } from './datasource';
import { PromQuery } from './types';

/**
 * Number of time series results needed before starting to suggest sum aggregation hints
 */
export const SUM_HINT_THRESHOLD_COUNT = 20;

export function getQueryHints(
  promQuery: PromQuery,
  series?: any[],
  datasource?: PrometheusDatasource,
  timeRange?: TimeRange
): QueryHint[] {
  const hints = [];

  // ..._bucket metric needs a histogram_quantile()
  const query = promQuery.expr || '';
  const histogramMetric = query.trim().match(/^\w+_bucket$/);
  if (histogramMetric) {
    const label = 'Time series has buckets, you probably wanted a histogram.';
    hints.push({
      type: 'HISTOGRAM_QUANTILE',
      label,
      fix: {
        label: 'Fix by adding histogram_quantile().',
        action: {
          type: 'ADD_HISTOGRAM_QUANTILE',
          query,
        },
      } as QueryFix,
    });
  }

  // Check for need of rate()
  if (query.indexOf('rate(') === -1 && query.indexOf('increase(') === -1) {
    // Use metric metadata for exact types
    const nameMatch = query.match(/\b(\w+_(total|sum|count))\b/);
    let counterNameMetric = nameMatch ? nameMatch[1] : '';
    const metricsMetadata = datasource?.languageProvider?.metricsMetadata ?? {};
    const metricMetadataKeys = Object.keys(metricsMetadata);
    let certain = false;

    if (metricMetadataKeys.length > 0) {
      counterNameMetric =
        metricMetadataKeys.find((metricName) => {
          // Only considering first type information, could be non-deterministic
          const metadata = metricsMetadata[metricName][0];
          if (metadata.type.toLowerCase() === 'counter') {
            const metricRegex = new RegExp(`\\b${metricName}\\b`);
            if (query.match(metricRegex)) {
              certain = true;
              return true;
            }
          }
          return false;
        }) ?? '';
    }

    if (counterNameMetric) {
      const simpleMetric = query.trim().match(/^\w+$/);
      const verb = certain ? 'is' : 'looks like';
      let label = `Metric ${counterNameMetric} ${verb} a counter.`;
      let fix: QueryFix | undefined;

      if (simpleMetric) {
        fix = {
          label: 'Fix by adding rate().',
          action: {
            type: 'ADD_RATE',
            query,
          },
        };
      } else {
        label = `${label} Try applying a rate() function.`;
      }

      hints.push({
        type: 'APPLY_RATE',
        label,
        fix,
      });
    }
  }

  // Check for recording rules expansion
  if (datasource && datasource.ruleMappings) {
    const mapping = datasource.ruleMappings;
    const mappingForQuery = Object.keys(mapping).reduce((acc, ruleName) => {
      if (query.search(ruleName) > -1) {
        return {
          ...acc,
          [ruleName]: mapping[ruleName],
        };
      }
      return acc;
    }, {});
    if (size(mappingForQuery) > 0) {
      const label = 'Query contains recording rules.';
      hints.push({
        type: 'EXPAND_RULES',
        label,
        fix: ({
          label: 'Expand rules',
          action: {
            type: 'EXPAND_RULES',
            query,
            mapping: mappingForQuery,
          },
        } as any) as QueryFix,
      });
    }
  }

  if (series && series.length >= SUM_HINT_THRESHOLD_COUNT) {
    const simpleMetric = query.trim().match(/^\w+$/);
    if (simpleMetric) {
      hints.push({
        type: 'ADD_SUM',
        label: 'Many time series results returned.',
        fix: {
          label: 'Consider aggregating with sum().',
          action: {
            type: 'ADD_SUM',
            query: query,
            preventSubmit: true,
          },
        } as QueryFix,
      });
    }
  }

  //Check if interval is below safeInterval
  if (timeRange) {
    const range = datasource?.getRange(timeRange);
    const safeInterval = datasource?.getSafeInterval(range ?? 0);
    const interval = rangeUtil.intervalToSeconds(promQuery.interval ?? '15s');
    let label =
      'The specified step interval is lower than the safe interval. Consider increasing the step interval or changing the time range';
    if (promQuery.stepMode === 'exact') {
      label =
        'The specified step interval is lower than the safe interval and has been changed to ' +
        safeInterval +
        's. Consider increasing the step interval or changing the time range';
    }
    if (safeInterval && interval < safeInterval) {
      hints.push({
        type: 'SAFE_INTERVAL',
        label,
      });
    }
  }

  return hints;
}

export function getInitHints(datasource: PrometheusDatasource): QueryHint[] {
  const hints = [];
  // Hint if using Loki as Prometheus data source
  if (datasource.directUrl.includes('/loki') && !datasource.languageProvider.metrics.length) {
    hints.push({
      label: `Using Loki as a Prometheus data source is no longer supported. You must use the Loki data source for your Loki instance.`,
      type: 'INFO',
    });
  }

  // Hint for big disabled lookups
  if (datasource.lookupsDisabled) {
    hints.push({
      label: `Labels and metrics lookup was disabled in data source settings.`,
      type: 'INFO',
    });
  }

  return hints;
}
