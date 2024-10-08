import React from 'react';
import TokensTable from '../../components/TokensTable';
import { Banner, Layout } from '@douyinfe/semi-ui';
const Token = () => (
  <>
    <Layout>
      <Layout.Header>
        <Banner
          type='warning'
          description='令牌无法精确控制使用额度，请勿直接将令牌分发给用户。'
        />
        <Banner
          type="success"
          description='点击“聊天”直接访问Web站点进行对话。'
        />
      </Layout.Header>
      <Layout.Content>
        <TokensTable />
      </Layout.Content>
    </Layout>
  </>
);

export default Token;
