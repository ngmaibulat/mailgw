'use strict';
module.exports = {
  async up(queryInterface, Sequelize) {
    await queryInterface.createTable('HashLookups', {
      id: {
        allowNull: false,
        autoIncrement: true,
        primaryKey: true,
        type: Sequelize.INTEGER
      },
      txn_uuid: {
        type: Sequelize.STRING
      },
      md5: {
        type: Sequelize.STRING
      },
      contentType: {
        type: Sequelize.STRING
      },
      filename: {
        type: Sequelize.STRING
      },
      size: {
        type: Sequelize.INTEGER
      },
      action: {
        type: Sequelize.STRING
      },
      createdAt: {
        allowNull: false,
        type: Sequelize.DATE
      },
      updatedAt: {
        allowNull: false,
        type: Sequelize.DATE
      }
    });
  },
  async down(queryInterface, Sequelize) {
    await queryInterface.dropTable('HashLookups');
  }
};